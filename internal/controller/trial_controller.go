/*
Copyright 2026 The Parallax Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/aburan28/parallax/api/v1alpha1"
	"github.com/aburan28/parallax/internal/store"
	pluginv1 "github.com/aburan28/parallax/proto/plugin/v1"
)

// pluginCallTimeout bounds a single plugin RPC so a wedged plugin cannot wedge the
// reconcile loop (DESIGN.md §5.5). Deep per-call deadlines/circuit-breaking is TODO(m1).
const pluginCallTimeout = 30 * time.Second

// trialRequeue is the short delay between state-machine steps; one transition happens
// per reconcile so each phase boundary is observable and idempotent.
const trialRequeue = 2 * time.Second

// maxTrialRequeue caps how long the trial sleeps while waiting out a warmup or
// measurement window. Long windows still get polled often enough that a driver abort
// is noticed promptly rather than at the end of the window.
const maxTrialRequeue = 15 * time.Second

// healthGateTimeout bounds how long the HealthGate phase waits for target.Ready to
// report ready before the trial is failed (DESIGN.md §9).
const healthGateTimeout = 5 * time.Minute

// TrialReconciler executes one Trial CR through the state machine (DESIGN.md §9),
// calling plugins at the right boundaries and persisting evidence to the results DB.
type TrialReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Deps
	// Plugins is the plugin call surface; when nil it is built from Deps.Host.
	// Tests inject a fake.
	Plugins trialPlugins
}

// plugins returns the plugin call surface, defaulting to a host-backed adapter.
func (r *TrialReconciler) plugins() trialPlugins {
	if r.Plugins != nil {
		return r.Plugins
	}
	return hostPlugins{host: r.Host}
}

// resolvedPlugins holds the plugin names a trial calls, resolved from its parent Study.
type resolvedPlugins struct {
	target     string
	loadDriver string
	providers  []string
}

// trialContext is what a reconcile needs from the trial's parent Study: the study
// itself, the workload this trial runs, and the plugins to call. The workload is what
// makes the trial clock work — it carries the window durations and who closes them.
type trialContext struct {
	study    *v1alpha1.Study
	workload *v1alpha1.Workload
	plugins  resolvedPlugins
}

// warmup is the unmeasured run-in before t1. The trial's own fidelity dial wins over
// the workload's default, so screening and validation reps can differ (§7).
func (tc trialContext) warmup(trial *v1alpha1.Trial) time.Duration {
	if d := trial.Spec.Fidelity.Warmup.Duration; d > 0 {
		return d
	}
	if tc.workload != nil {
		return tc.workload.Warmup.Duration
	}
	return 0
}

// measure is the measurement window for completion=duration workloads.
func (tc trialContext) measure(trial *v1alpha1.Trial) time.Duration {
	if d := trial.Spec.Fidelity.Measure.Duration; d > 0 {
		return d
	}
	if tc.workload != nil {
		return tc.workload.Measure.Duration
	}
	return 0
}

// maxDuration caps the measurement window in either completion mode; 0 means uncapped.
func (tc trialContext) maxDuration() time.Duration {
	if tc.workload == nil {
		return 0
	}
	return tc.workload.MaxDuration.Duration
}

// cooldown quiesces the system after t2, before collection.
func (tc trialContext) cooldown() time.Duration {
	if tc.workload == nil {
		return 0
	}
	return tc.workload.Cooldown.Duration
}

// driverCompleted reports whether this workload's window is closed by the driver
// reporting done rather than by a fixed duration elapsing (docs/GENERALIZATION.md G1).
func (tc trialContext) driverCompleted() bool {
	return tc.workload != nil && tc.workload.Completion == v1alpha1.CompletionDriver
}

// requeueWithin turns a remaining wait into a requeue delay: never busier than
// trialRequeue, never sleepier than maxTrialRequeue.
func requeueWithin(remaining time.Duration) time.Duration {
	switch {
	case remaining < trialRequeue:
		return trialRequeue
	case remaining > maxTrialRequeue:
		return maxTrialRequeue
	default:
		return remaining
	}
}

// +kubebuilder:rbac:groups=parallax.dev,resources=trials,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=parallax.dev,resources=trials/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=parallax.dev,resources=trials/finalizers,verbs=update
// +kubebuilder:rbac:groups=parallax.dev,resources=studies,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile advances the trial one state-machine step per call:
//
//	Pending -> Configuring -> HealthGate -> Warmup -> Measuring -> Draining -> Collecting -> Collected
//
// stamping Status.Timeline at each boundary and, on collection, persisting a TrialRecord
// plus SLI values. Plugin RPCs are best-effort in M0 (plugins may be unwired); the deep
// behaviour behind each call is TODO(m1).
func (r *TrialReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var trial v1alpha1.Trial
	if err := r.Get(ctx, req.NamespacedName, &trial); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !trial.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil // owned by Study; GC handles removal.
	}

	if trial.Status.Phase == "" {
		trial.Status.Phase = v1alpha1.TrialPhasePending
	}
	trial.Status.ObservedGeneration = trial.Generation

	// Resolve the parent Study to find the workload and plugins this trial drives.
	// Absence is not fatal in M0 — the state machine still runs, plugin calls are
	// simply skipped.
	tc := r.resolveContext(ctx, &trial)
	pl, study := tc.plugins, tc.study

	now := func() *metav1.Time { t := metav1.Now(); return &t }

	// One bounded context for whatever plugin call this reconcile makes (§5.5).
	cctx, cancel := context.WithTimeout(ctx, pluginCallTimeout)
	defer cancel()

	switch trial.Status.Phase {
	case v1alpha1.TrialPhasePending:
		r.Recorder.Event(&trial, corev1.EventTypeNormal, "Started", "trial state machine started")
		trial.Status.Phase = v1alpha1.TrialPhaseConfiguring

	case v1alpha1.TrialPhaseConfiguring:
		// target.Apply materializes the config point (helm upgrade / CR patch). A
		// reachable target that reports failure fails the trial; an unreachable plugin
		// (no host wired) is tolerated so --local runs without plugins still progress.
		if pl.target != "" {
			resp, err := r.plugins().Apply(cctx, pl.target, &pluginv1.ApplyRequest{
				ConfigHash:    trial.Spec.ConfigHash,
				Assignments:   assignments(tc, &trial),
				RawConfigJson: rawConfig(&trial),
			})
			if err != nil {
				r.pluginSkipped(ctx, &trial, "TargetApply", pl.target, err)
			} else if !resp.GetApplied() {
				return r.failTrial(ctx, &trial, v1alpha1.TrialPhaseFailed, "TargetApplyFailed", resp.GetDetail())
			}
		}
		trial.Status.Timeline.Applied = now()
		trial.Status.Phase = v1alpha1.TrialPhaseHealthGate

	case v1alpha1.TrialPhaseHealthGate:
		// target.Ready gates on rollout + health before load starts. A not-ready gate
		// requeues until the health-gate deadline, then fails (§9).
		if pl.target != "" {
			resp, err := r.plugins().Ready(cctx, pl.target, &pluginv1.ReadyRequest{
				ConfigHash:     trial.Spec.ConfigHash,
				TimeoutSeconds: int64(healthGateTimeout / time.Second),
			})
			switch {
			case err != nil:
				r.pluginSkipped(ctx, &trial, "TargetReady", pl.target, err)
			case !resp.GetReady():
				if since(trial.Status.Timeline.Applied) > healthGateTimeout {
					return r.failTrial(ctx, &trial, v1alpha1.TrialPhaseFailed, "HealthGateTimeout", resp.GetDetail())
				}
				return ctrl.Result{RequeueAfter: trialRequeue}, r.Status().Update(ctx, &trial) // stay in HealthGate
			}
		}
		trial.Status.Timeline.Ready = now()
		trial.Status.Phase = v1alpha1.TrialPhaseWarmup

	case v1alpha1.TrialPhaseWarmup:
		// Start the load exactly once, then hold here for the warmup — run-in traffic
		// is deliberately not measured. The driver's opaque handle is recorded on
		// status so later reconciles drive the same run; the ABI never requires a
		// driver to key its runs by trial id (docs/GENERALIZATION.md G5).
		if trial.Status.Timeline.LoadStarted == nil {
			if pl.loadDriver != "" {
				resp, err := r.plugins().StartLoad(cctx, pl.loadDriver, &pluginv1.StartRequest{
					TrialId:      string(trial.UID),
					WorkloadJson: workloadJSON(tc, &trial),
				})
				if err != nil {
					r.pluginSkipped(ctx, &trial, "LoadStart", pl.loadDriver, err)
				} else {
					trial.Status.Load.RunRef = resp.GetRunRef()
				}
			}
			trial.Status.Timeline.LoadStarted = now()
		}
		if elapsed, warm := since(trial.Status.Timeline.LoadStarted), tc.warmup(&trial); elapsed < warm {
			if err := r.Status().Update(ctx, &trial); err != nil {
				return ctrl.Result{}, fmt.Errorf("update warming trial status: %w", err)
			}
			return ctrl.Result{RequeueAfter: requeueWithin(warm - elapsed)}, nil
		}
		// t1 is stamped as measurement begins (after warmup) — §9 "timestamps are law".
		trial.Status.Timeline.T1 = now()
		trial.Status.Phase = v1alpha1.TrialPhaseMeasuring

	case v1alpha1.TrialPhaseMeasuring:
		// Hold the window open until whatever closes it says so: a fixed duration for
		// continuous workloads, or the driver reporting done for run-to-completion
		// ones. Either way the driver is polled each reconcile so its abort policy is
		// enforced by the core rather than merely declared (G1, G5).
		elapsed := since(trial.Status.Timeline.T1)
		done, aborted, reason := r.pollLoad(cctx, pl.loadDriver, &trial)
		switch {
		case aborted:
			trial.Status.Load.Aborted = true
			trial.Status.Load.AbortReason = reason
			r.stopLoad(cctx, pl.loadDriver, &trial)
			return r.failTrial(ctx, &trial, v1alpha1.TrialPhaseAborted, "LoadAborted", reason)

		case !windowClosed(tc, &trial, elapsed, done):
			if max := tc.maxDuration(); max > 0 && elapsed >= max {
				r.stopLoad(cctx, pl.loadDriver, &trial)
				return r.failTrial(ctx, &trial, v1alpha1.TrialPhaseAborted, "MeasureDeadlineExceeded",
					fmt.Sprintf("measurement window hit maxDuration %s before closing; the trial is not comparable", max))
			}
			if err := r.Status().Update(ctx, &trial); err != nil {
				return ctrl.Result{}, fmt.Errorf("update measuring trial status: %w", err)
			}
			return ctrl.Result{RequeueAfter: requeueWithin(tc.measure(&trial) - elapsed)}, nil
		}

		// Window closes: stamp t2 first so nothing after this point can widen it.
		trial.Status.Timeline.T2 = now()
		if ok := r.stopLoad(cctx, pl.loadDriver, &trial); !ok {
			return r.failTrial(ctx, &trial, v1alpha1.TrialPhaseAborted, "LoadStopFailed",
				"load driver did not report a clean stop")
		}
		trial.Status.Phase = v1alpha1.TrialPhaseDraining

	case v1alpha1.TrialPhaseDraining:
		// Drain before collect: buffers flush before readback (§9). TODO(m1): gate on
		// a driver/target-reported quiesce signal rather than the cooldown alone.
		if elapsed, cool := since(trial.Status.Timeline.T2), tc.cooldown(); elapsed < cool {
			if err := r.Status().Update(ctx, &trial); err != nil {
				return ctrl.Result{}, fmt.Errorf("update draining trial status: %w", err)
			}
			return ctrl.Result{RequeueAfter: requeueWithin(cool - elapsed)}, nil
		}
		trial.Status.Timeline.Drained = now()
		trial.Status.Phase = v1alpha1.TrialPhaseCollecting

	case v1alpha1.TrialPhaseCollecting:
		// provider.Collect evaluates SLIs strictly inside [t1, t2], then evidence is
		// persisted to the results DB.
		if err := r.collectAndPersist(ctx, pl, study, &trial); err != nil {
			return ctrl.Result{}, err
		}
		apimeta.SetStatusCondition(&trial.Status.Conditions, metav1.Condition{
			Type:               condReady,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: trial.Generation,
			Reason:             "Collected",
			Message:            "trial completed and evidence persisted",
		})
		r.Recorder.Event(&trial, corev1.EventTypeNormal, "Collected", "SLIs and artifacts persisted")
		trial.Status.Phase = v1alpha1.TrialPhaseCollected

	case v1alpha1.TrialPhaseCollected, v1alpha1.TrialPhaseFailed,
		v1alpha1.TrialPhaseAborted, v1alpha1.TrialPhaseInvalid:
		// Terminal: nothing to do.
		if err := r.Status().Update(ctx, &trial); err != nil {
			return ctrl.Result{}, fmt.Errorf("update terminal trial status: %w", err)
		}
		return ctrl.Result{}, nil

	default:
		return ctrl.Result{}, fmt.Errorf("trial %q in unknown phase %q", trial.Name, trial.Status.Phase)
	}

	if err := r.Status().Update(ctx, &trial); err != nil {
		return ctrl.Result{}, fmt.Errorf("update trial status: %w", err)
	}
	log.Info("advanced trial", "phase", trial.Status.Phase)
	return ctrl.Result{RequeueAfter: trialRequeue}, nil
}

// resolveContext fetches the parent Study (best-effort) and maps its spec onto the
// workload and plugin names this trial drives.
func (r *TrialReconciler) resolveContext(ctx context.Context, trial *v1alpha1.Trial) trialContext {
	log := logf.FromContext(ctx)
	var study v1alpha1.Study
	if err := r.Get(ctx, client.ObjectKey{Namespace: trial.Namespace, Name: trial.Spec.StudyRef.Name}, &study); err != nil {
		if !apierrors.IsNotFound(err) {
			log.Error(err, "get parent study", "study", trial.Spec.StudyRef.Name)
		}
		return trialContext{}
	}
	tc := trialContext{study: &study, plugins: resolvedPlugins{target: study.Spec.Target.Plugin}}
	// The load driver is named by the workload, independently of what it drives (§8).
	for i := range study.Spec.Workloads {
		if study.Spec.Workloads[i].Name == trial.Spec.Workload {
			tc.workload = &study.Spec.Workloads[i]
			tc.plugins.loadDriver = tc.workload.Driver.Plugin
			break
		}
	}
	seen := map[string]struct{}{}
	for _, sli := range study.Spec.SLIs {
		if sli.From == nil || sli.From.Plugin == "" {
			continue
		}
		if _, dup := seen[sli.From.Plugin]; dup {
			continue
		}
		seen[sli.From.Plugin] = struct{}{}
		tc.plugins.providers = append(tc.plugins.providers, sli.From.Plugin)
	}
	return tc
}

// windowClosed reports whether the measurement window should close now: the driver
// says so for completion=driver workloads, otherwise the measure duration has run out.
// A zero measure closes immediately, which is why the CRD defaults it.
func windowClosed(tc trialContext, trial *v1alpha1.Trial, elapsed time.Duration, driverDone bool) bool {
	if tc.driverCompleted() {
		return driverDone
	}
	return elapsed >= tc.measure(trial)
}

// pollLoad asks the driver how the run is going. An unreachable driver is reported as
// neither done nor aborted, so a plugin outage cannot silently truncate a window.
func (r *TrialReconciler) pollLoad(ctx context.Context, driver string, trial *v1alpha1.Trial) (done, aborted bool, reason string) {
	if driver == "" || trial.Status.Load.RunRef == "" {
		return false, false, ""
	}
	resp, err := r.plugins().Progress(ctx, driver, &pluginv1.ProgressRequest{RunRef: trial.Status.Load.RunRef})
	if err != nil {
		r.pluginSkipped(ctx, trial, "LoadProgress", driver, err)
		return false, false, ""
	}
	return resp.GetDone(), resp.GetAborted(), resp.GetAbortReason()
}

// stopLoad ends the driver run and records its summary metrics on status, where
// `driver`-sourced SLIs read them. It reports whether the driver stopped cleanly; an
// unreachable driver counts as clean so an unwired plugin cannot fail every trial.
func (r *TrialReconciler) stopLoad(ctx context.Context, driver string, trial *v1alpha1.Trial) bool {
	if driver == "" || trial.Status.Load.RunRef == "" {
		return true
	}
	resp, err := r.plugins().StopLoad(ctx, driver, &pluginv1.StopRequest{RunRef: trial.Status.Load.RunRef})
	if err != nil {
		r.pluginSkipped(ctx, trial, "LoadStop", driver, err)
		return true
	}
	if m := resp.GetMetrics(); len(m) > 0 {
		metrics := make(map[string]string, len(m))
		for k, v := range m {
			metrics[k] = strconv.FormatFloat(v, 'g', -1, 64)
		}
		trial.Status.Load.Metrics = metrics
	}
	return resp.GetOk()
}

// assignments renders the trial's resolved config point for target.Apply, pairing each
// dimension value with the target config path the study mapped it to. Path defaults to
// the dimension name, preserving the "names are target paths" convention (§8, G4).
func assignments(tc trialContext, trial *v1alpha1.Trial) []*pluginv1.DimensionAssignment {
	if len(trial.Spec.Dimensions) == 0 {
		return nil
	}
	// Index the study's declared dimensions so each assignment carries its mapping.
	declared := map[string]*v1alpha1.Dimension{}
	if tc.study != nil {
		for i := range tc.study.Spec.Space.Dimensions {
			d := &tc.study.Spec.Space.Dimensions[i]
			declared[d.Name] = d
		}
	}
	names := make([]string, 0, len(trial.Spec.Dimensions))
	for name := range trial.Spec.Dimensions {
		names = append(names, name)
	}
	sort.Strings(names) // stable request ordering for reproducible plugin behaviour
	out := make([]*pluginv1.DimensionAssignment, 0, len(names))
	for _, name := range names {
		a := &pluginv1.DimensionAssignment{Name: name, Value: trial.Spec.Dimensions[name], Path: name}
		if d, ok := declared[name]; ok {
			if d.Path != "" {
				a.Path = d.Path
			}
			if d.Mapping != nil {
				a.MappingJson = d.Mapping.Raw
			}
		}
		out = append(out, a)
	}
	return out
}

// pluginSkipped records that a plugin call could not be made (typically because the
// plugin is not wired/Ready in an M0 environment) without failing the trial.
func (r *TrialReconciler) pluginSkipped(ctx context.Context, trial *v1alpha1.Trial, reason, name string, err error) {
	logf.FromContext(ctx).Info("plugin call skipped (plugin may be unwired)", "reason", reason, "plugin", name, "err", err)
	r.Recorder.Eventf(trial, corev1.EventTypeWarning, reason, "plugin %q unavailable: %v", name, err)
}

// failTrial transitions a trial to a terminal non-success phase (Failed/Aborted),
// records the reason, and persists status. It returns a no-requeue result.
func (r *TrialReconciler) failTrial(ctx context.Context, trial *v1alpha1.Trial, phase v1alpha1.TrialPhase, reason, detail string) (ctrl.Result, error) {
	trial.Status.Phase = phase
	apimeta.SetStatusCondition(&trial.Status.Conditions, metav1.Condition{
		Type:               condReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: trial.Generation,
		Reason:             reason,
		Message:            detail,
	})
	r.Recorder.Eventf(trial, corev1.EventTypeWarning, reason, "trial %s: %s", phase, detail)
	if err := r.Status().Update(ctx, trial); err != nil {
		return ctrl.Result{}, fmt.Errorf("update failed-trial status: %w", err)
	}
	return ctrl.Result{}, nil
}

// collectAndPersist evaluates SLIs via provider plugins over the recorded window and
// writes the TrialRecord plus SLI values to the results DB (DESIGN.md §11, §15).
func (r *TrialReconciler) collectAndPersist(ctx context.Context, pl resolvedPlugins, study *v1alpha1.Study, trial *v1alpha1.Trial) error {
	log := logf.FromContext(ctx)

	collected := map[string]collectedSLI{}
	if study != nil {
		collected = r.collectSLIs(ctx, study, trial)
	}

	// Status summary carries the collected value per SLI (0 where a provider was
	// unreachable, so the trial still completes rather than wedging).
	var sliResults []v1alpha1.SLIResult
	if study != nil {
		for _, sli := range study.Spec.SLIs {
			c := collected[sli.Name]
			sliResults = append(sliResults, v1alpha1.SLIResult{
				Name:  sli.Name,
				Class: sli.Class,
				Value: strconv.FormatFloat(c.value, 'g', -1, 64),
			})
		}
	}
	trial.Status.SLIs = sliResults

	// Persist a TrialRecord + SLI values keyed to the parent run (§15).
	runID, ok := parseInt64(runIDFromStudy(study))
	if !ok {
		log.Info("skipping results-DB persistence: no run id on parent study")
		return nil
	}

	timelineJSON, err := json.Marshal(trial.Status.Timeline)
	if err != nil {
		return fmt.Errorf("marshal trial timeline: %w", err)
	}
	rec := &store.TrialRecord{
		RunID:      runID,
		ConfigHash: trial.Spec.ConfigHash,
		Workload:   trial.Spec.Workload,
		Rep:        int(trial.Spec.Rep),
		Mode:       string(trial.Spec.Mode),
		Phase:      string(v1alpha1.TrialPhaseCollected),
		Validity:   "valid",
		Timeline:   timelineJSON,
		CreatedAt:  time.Now().UTC(),
	}
	if trial.Spec.Config != nil {
		rec.Config = trial.Spec.Config.Raw
	}
	trialID, err := r.Store.UpsertTrial(ctx, rec)
	if err != nil {
		return fmt.Errorf("upsert trial record: %w", err)
	}

	if study != nil && len(study.Spec.SLIs) > 0 {
		values := make([]store.SLIValueRecord, 0, len(study.Spec.SLIs))
		for _, sli := range study.Spec.SLIs {
			c := collected[sli.Name]
			values = append(values, store.SLIValueRecord{
				TrialID:     trialID,
				Name:        sli.Name,
				Provider:    sliProvider(sli),
				Class:       sli.Class,
				Value:       c.value,
				Query:       sliQueryEvidence(sli),
				EvaluatedAt: time.Now().UTC(),
			})
		}
		if err := r.Store.InsertSLIValues(ctx, values); err != nil {
			return fmt.Errorf("insert SLI values: %w", err)
		}
	}

	trial.Status.Run = v1alpha1.RunRef{
		RunID:   runIDFromStudy(study),
		TrialID: fmt.Sprintf("%d", trialID),
	}
	return nil
}

// collectedSLI is one provider-returned SLI value plus whether the provider answered.
type collectedSLI struct {
	value float64
	ok    bool
}

// collectSLIs resolves every SLI the study declares, from whichever of the three
// sources it names (docs/GENERALIZATION.md G6):
//
//   - driver — the load driver's summary metrics, already on status after Stop. No
//     plugin call, and no assumption about what the driver measures.
//   - from   — a provider plugin, called once per provider over [t1, t2] with the
//     study's provider-native config block passed through verbatim.
//   - derived — computed in-core from the values above. TODO(m1): expression
//     evaluation; these resolve as not-ok until then.
func (r *TrialReconciler) collectSLIs(ctx context.Context, study *v1alpha1.Study, trial *v1alpha1.Trial) map[string]collectedSLI {
	out := map[string]collectedSLI{}
	window := &pluginv1.Window{
		T1Rfc3339: timeRFC3339(trial.Status.Timeline.T1),
		T2Rfc3339: timeRFC3339(trial.Status.Timeline.T2),
	}

	// provider name -> the SLIQuery list to ask it for.
	byProvider := map[string][]*pluginv1.SLIQuery{}
	for _, sli := range study.Spec.SLIs {
		switch {
		case sli.Driver != nil:
			raw, ok := trial.Status.Load.Metrics[sli.Driver.Metric]
			if !ok {
				logf.FromContext(ctx).Info("load driver reported no such metric",
					"sli", sli.Name, "metric", sli.Driver.Metric)
				continue
			}
			v, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				logf.FromContext(ctx).Error(err, "parse driver metric", "sli", sli.Name, "raw", raw)
				continue
			}
			out[sli.Name] = collectedSLI{value: v, ok: true}

		case sli.From != nil && sli.From.Plugin != "":
			byProvider[sli.From.Plugin] = append(byProvider[sli.From.Plugin], &pluginv1.SLIQuery{
				Name:       sli.Name,
				SliClass:   sli.Class,
				ConfigJson: rawExtension(sli.From.Config),
			})
		}
	}

	for provider, queries := range byProvider {
		cctx, cancel := context.WithTimeout(ctx, pluginCallTimeout)
		resp, err := r.plugins().Collect(cctx, provider, &pluginv1.CollectRequest{
			Window:  window,
			Queries: queries,
		})
		cancel()
		if err != nil {
			r.pluginSkipped(ctx, trial, "ProviderCollect", provider, err)
			continue
		}
		for _, v := range resp.GetValues() {
			out[v.GetName()] = collectedSLI{value: v.GetValue(), ok: v.GetOk()}
		}
	}
	return out
}

// sliProvider names the source recorded alongside an SLI value in the results DB.
func sliProvider(sli v1alpha1.SLISpec) string {
	switch {
	case sli.From != nil:
		return sli.From.Plugin
	case sli.Driver != nil:
		return "driver"
	case sli.Derived != nil:
		return "derived"
	default:
		return ""
	}
}

// sliQueryEvidence is the query text recorded with an SLI value: the provider config
// block, the driver metric key, or the derived expression.
func sliQueryEvidence(sli v1alpha1.SLISpec) string {
	switch {
	case sli.From != nil && sli.From.Config != nil:
		return string(sli.From.Config.Raw)
	case sli.Driver != nil:
		return sli.Driver.Metric
	case sli.Derived != nil:
		return sli.Derived.Expr
	default:
		return ""
	}
}

// rawExtension returns a RawExtension's bytes, or nil.
func rawExtension(ext *runtime.RawExtension) []byte {
	if ext == nil {
		return nil
	}
	return ext.Raw
}

// rawConfig returns the trial's config JSON, or nil.
func rawConfig(trial *v1alpha1.Trial) []byte {
	if trial.Spec.Config != nil {
		return trial.Spec.Config.Raw
	}
	return nil
}

// workloadJSON serializes the resolved workload block the load driver runs, with the
// trial's fidelity overlay attached so a screening rep can run the same driver at a
// lower intensity than a validation rep. The driver owns the merge: only it knows
// which of its own fields the overlay may touch.
func workloadJSON(tc trialContext, trial *v1alpha1.Trial) []byte {
	if tc.workload == nil {
		return nil
	}
	payload := struct {
		v1alpha1.Workload `json:",inline"`
		Fidelity          v1alpha1.FidelitySpec `json:"fidelity,omitempty"`
		TrialID           string                `json:"trialID,omitempty"`
	}{
		Workload: *tc.workload,
		Fidelity: trial.Spec.Fidelity,
		TrialID:  string(trial.UID),
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return nil
	}
	return b
}

// timeRFC3339 renders a metav1.Time as RFC3339, or "" if nil.
func timeRFC3339(t *metav1.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// since returns how long ago t was, or 0 if t is nil.
func since(t *metav1.Time) time.Duration {
	if t == nil {
		return 0
	}
	return time.Since(t.Time)
}

// runIDFromStudy returns the results-DB run id string from the study status, or "".
func runIDFromStudy(study *v1alpha1.Study) string {
	if study == nil {
		return ""
	}
	return study.Status.RunID
}

// SetupWithManager registers the reconciler for Trial objects.
func (r *TrialReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Trial{}).
		Named("trial").
		Complete(r)
}

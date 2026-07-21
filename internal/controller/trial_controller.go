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

// healthGateTimeout bounds how long the HealthGate phase waits for target.Ready to
// report ready before the trial is failed (DESIGN.md §9).
const healthGateTimeout = 5 * time.Minute

// loadRunRefAnnotation stores the load driver's run handle between Start and Stop so
// the reconcile that stops the load can reference the run the earlier reconcile began.
const loadRunRefAnnotation = "parallax.dev/load-run-ref"

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

	// Resolve the parent Study to find the plugins this trial drives. Absence is not
	// fatal in M0 — the state machine still runs, plugin calls are simply skipped.
	pl, study := r.resolvePlugins(ctx, &trial)

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
				Dimensions:    trial.Spec.Dimensions,
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
		// loaddriver.Start begins replay; warmup is not measured. The run is keyed by
		// trial UID so a later reconcile can Stop it without cross-reconcile state.
		if pl.loadDriver != "" {
			if _, err := r.plugins().StartLoad(cctx, pl.loadDriver, &pluginv1.StartRequest{
				TrialId:      string(trial.UID),
				WorkloadJson: workloadJSON(study, &trial),
			}); err != nil {
				r.pluginSkipped(ctx, &trial, "LoadStart", pl.loadDriver, err)
			}
		}
		// t1 is stamped as measurement begins (after warmup) — §9 "timestamps are law".
		trial.Status.Timeline.T1 = now()
		trial.Status.Phase = v1alpha1.TrialPhaseMeasuring

	case v1alpha1.TrialPhaseMeasuring:
		// Measurement window closes: stamp t2 and stop the load driver. An abort reported
		// by the driver terminates the trial as Aborted (recorded, §9).
		trial.Status.Timeline.T2 = now()
		if pl.loadDriver != "" {
			resp, err := r.plugins().StopLoad(cctx, pl.loadDriver, &pluginv1.StopRequest{
				RunRef: string(trial.UID),
			})
			if err != nil {
				r.pluginSkipped(ctx, &trial, "LoadStop", pl.loadDriver, err)
			} else if resp != nil && !resp.GetOk() {
				return r.failTrial(ctx, &trial, v1alpha1.TrialPhaseAborted, "LoadAborted", "load driver reported an aborted run")
			}
		}
		trial.Status.Phase = v1alpha1.TrialPhaseDraining

	case v1alpha1.TrialPhaseDraining:
		// Drain before collect: buffers flush before readback (§9). TODO(m1): gate on
		// capture_agent_write_queue_depth == 0 and max(flushInterval)+margin elapsed.
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

// resolvePlugins fetches the parent Study (best-effort) and maps its spec onto the
// plugin names this trial drives.
func (r *TrialReconciler) resolvePlugins(ctx context.Context, trial *v1alpha1.Trial) (resolvedPlugins, *v1alpha1.Study) {
	log := logf.FromContext(ctx)
	var study v1alpha1.Study
	if err := r.Get(ctx, client.ObjectKey{Namespace: trial.Namespace, Name: trial.Spec.StudyRef.Name}, &study); err != nil {
		if !apierrors.IsNotFound(err) {
			log.Error(err, "get parent study", "study", trial.Spec.StudyRef.Name)
		}
		return resolvedPlugins{}, nil
	}
	pl := resolvedPlugins{target: study.Spec.Target.Plugin}
	// The load driver plugin is the workload's replay engine (§8, App. A.2).
	for i := range study.Spec.Workloads {
		if study.Spec.Workloads[i].Name == trial.Spec.Workload {
			pl.loadDriver = study.Spec.Workloads[i].Replay.Engine
			break
		}
	}
	seen := map[string]struct{}{}
	for _, sli := range study.Spec.SLIs {
		if sli.Provider == "" {
			continue
		}
		if _, dup := seen[sli.Provider]; dup {
			continue
		}
		seen[sli.Provider] = struct{}{}
		pl.providers = append(pl.providers, sli.Provider)
	}
	return pl, &study
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
				Provider:    sli.Provider,
				Class:       sli.Class,
				Value:       c.value,
				Query:       sli.Query,
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

// collectSLIs groups the study's provider-backed SLIs by provider, calls each
// provider's Collect over the trial's measurement window, and returns the values by
// SLI name. The derived class is computed in-core, not via a provider (§5.1); derived
// evaluation is TODO(m1), so those SLIs are collected as zero for now.
func (r *TrialReconciler) collectSLIs(ctx context.Context, study *v1alpha1.Study, trial *v1alpha1.Trial) map[string]collectedSLI {
	out := map[string]collectedSLI{}
	window := &pluginv1.Window{
		T1Rfc3339: timeRFC3339(trial.Status.Timeline.T1),
		T2Rfc3339: timeRFC3339(trial.Status.Timeline.T2),
	}

	// provider name -> the SLIQuery list to ask it for.
	byProvider := map[string][]*pluginv1.SLIQuery{}
	for _, sli := range study.Spec.SLIs {
		if sli.Provider == "" || sli.Class == "derived" {
			continue // derived SLIs are in-core (TODO(m1)); unprovidered SLIs skipped
		}
		byProvider[sli.Provider] = append(byProvider[sli.Provider], &pluginv1.SLIQuery{
			Name:     sli.Name,
			SliClass: sli.Class,
			Query:    sli.Query,
			Expr:     sli.Expr,
		})
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

// rawConfig returns the trial's config JSON, or nil.
func rawConfig(trial *v1alpha1.Trial) []byte {
	if trial.Spec.Config != nil {
		return trial.Spec.Config.Raw
	}
	return nil
}

// workloadJSON serializes the resolved workload block the load driver replays.
func workloadJSON(study *v1alpha1.Study, trial *v1alpha1.Trial) []byte {
	if study == nil {
		return nil
	}
	for i := range study.Spec.Workloads {
		if study.Spec.Workloads[i].Name == trial.Spec.Workload {
			if b, err := json.Marshal(study.Spec.Workloads[i]); err == nil {
				return b
			}
		}
	}
	return nil
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

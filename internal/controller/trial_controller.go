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

// TrialReconciler executes one Trial CR through the state machine (DESIGN.md §9),
// calling plugins at the right boundaries and persisting evidence to the results DB.
type TrialReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Deps
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

	switch trial.Status.Phase {
	case v1alpha1.TrialPhasePending:
		r.Recorder.Event(&trial, corev1.EventTypeNormal, "Started", "trial state machine started")
		trial.Status.Phase = v1alpha1.TrialPhaseConfiguring

	case v1alpha1.TrialPhaseConfiguring:
		// target.Apply materializes the config point (helm upgrade / CR patch).
		if err := r.applyTarget(ctx, pl.target, &trial); err != nil {
			r.pluginSkipped(ctx, &trial, "TargetApply", pl.target, err)
		}
		trial.Status.Timeline.Applied = now()
		trial.Status.Phase = v1alpha1.TrialPhaseHealthGate

	case v1alpha1.TrialPhaseHealthGate:
		// target.Ready gates on rollout + health before load starts.
		if err := r.readyTarget(ctx, pl.target, &trial); err != nil {
			r.pluginSkipped(ctx, &trial, "TargetReady", pl.target, err)
		}
		trial.Status.Timeline.Ready = now()
		trial.Status.Phase = v1alpha1.TrialPhaseWarmup

	case v1alpha1.TrialPhaseWarmup:
		// loaddriver.Start begins replay; warmup is not measured.
		if err := r.startLoad(ctx, pl.loadDriver, &trial); err != nil {
			r.pluginSkipped(ctx, &trial, "LoadStart", pl.loadDriver, err)
		}
		// t1 is stamped as measurement begins (after warmup) — §9 "timestamps are law".
		trial.Status.Timeline.T1 = now()
		trial.Status.Phase = v1alpha1.TrialPhaseMeasuring

	case v1alpha1.TrialPhaseMeasuring:
		// Measurement window closes: stamp t2 and stop the load driver.
		trial.Status.Timeline.T2 = now()
		if err := r.stopLoad(ctx, pl.loadDriver, &trial); err != nil {
			r.pluginSkipped(ctx, &trial, "LoadStop", pl.loadDriver, err)
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
	logf.FromContext(ctx).Info("plugin call skipped (M0: plugin may be unwired)", "reason", reason, "plugin", name, "err", err)
	r.Recorder.Eventf(trial, corev1.EventTypeWarning, reason, "plugin %q unavailable: %v", name, err)
}

// applyTarget calls target.Apply for the trial's config point.
func (r *TrialReconciler) applyTarget(ctx context.Context, name string, trial *v1alpha1.Trial) error {
	if name == "" {
		return fmt.Errorf("no target plugin resolved")
	}
	conn, err := r.Host.Conn(name)
	if err != nil {
		return fmt.Errorf("target plugin %q: %w", name, err)
	}
	cctx, cancel := context.WithTimeout(ctx, pluginCallTimeout)
	defer cancel()
	tc := pluginv1.NewTargetClient(conn)
	req := &pluginv1.ApplyRequest{
		ConfigHash: trial.Spec.ConfigHash,
		Dimensions: trial.Spec.Dimensions,
	}
	if trial.Spec.Config != nil {
		req.RawConfigJson = trial.Spec.Config.Raw
	}
	// TODO(m1): inspect ApplyResponse; on failure transition Trial -> Failed with reason.
	_, err = tc.Apply(cctx, req)
	return err
}

// readyTarget calls target.Ready to gate on rollout + health.
func (r *TrialReconciler) readyTarget(ctx context.Context, name string, trial *v1alpha1.Trial) error {
	if name == "" {
		return fmt.Errorf("no target plugin resolved")
	}
	conn, err := r.Host.Conn(name)
	if err != nil {
		return fmt.Errorf("target plugin %q: %w", name, err)
	}
	cctx, cancel := context.WithTimeout(ctx, pluginCallTimeout)
	defer cancel()
	tc := pluginv1.NewTargetClient(conn)
	// TODO(m1): honour ReadyResponse.ready; on gate timeout transition -> Failed.
	_, err = tc.Ready(cctx, &pluginv1.ReadyRequest{
		ConfigHash:     trial.Spec.ConfigHash,
		TimeoutSeconds: int64(pluginCallTimeout / time.Second),
	})
	return err
}

// startLoad calls loaddriver.Start to begin replay for this trial.
func (r *TrialReconciler) startLoad(ctx context.Context, name string, trial *v1alpha1.Trial) error {
	if name == "" {
		return fmt.Errorf("no load driver plugin resolved")
	}
	conn, err := r.Host.Conn(name)
	if err != nil {
		return fmt.Errorf("load driver plugin %q: %w", name, err)
	}
	cctx, cancel := context.WithTimeout(ctx, pluginCallTimeout)
	defer cancel()
	lc := pluginv1.NewLoadDriverClient(conn)
	// TODO(m1): build the workload JSON from the resolved Workload/Dataset and Watch the
	// stream for abort-policy state instead of fire-and-forget.
	_, err = lc.Start(cctx, &pluginv1.StartRequest{TrialId: string(trial.UID)})
	return err
}

// stopLoad calls loaddriver.Stop to end replay when the measurement window closes.
func (r *TrialReconciler) stopLoad(ctx context.Context, name string, trial *v1alpha1.Trial) error {
	if name == "" {
		return fmt.Errorf("no load driver plugin resolved")
	}
	conn, err := r.Host.Conn(name)
	if err != nil {
		return fmt.Errorf("load driver plugin %q: %w", name, err)
	}
	cctx, cancel := context.WithTimeout(ctx, pluginCallTimeout)
	defer cancel()
	lc := pluginv1.NewLoadDriverClient(conn)
	// TODO(m1): pass the real run ref returned by Start.
	_, err = lc.Stop(cctx, &pluginv1.StopRequest{RunRef: string(trial.UID)})
	return err
}

// collectAndPersist evaluates SLIs via provider plugins and writes the TrialRecord plus
// SLI values to the results DB.
func (r *TrialReconciler) collectAndPersist(ctx context.Context, pl resolvedPlugins, study *v1alpha1.Study, trial *v1alpha1.Trial) error {
	log := logf.FromContext(ctx)

	// Best-effort provider.Collect for each distinct provider. TODO(m1): build the
	// Window from [t1, t2] and the per-SLI queries, then parse CollectResponse into
	// concrete SLIValueRecords instead of the placeholders below.
	for _, name := range pl.providers {
		if err := r.collectProvider(ctx, name); err != nil {
			r.pluginSkipped(ctx, trial, "ProviderCollect", name, err)
		}
	}

	// Placeholder SLI results mirrored from the study spec (real values land in m1).
	var sliResults []v1alpha1.SLIResult
	if study != nil {
		for _, sli := range study.Spec.SLIs {
			sliResults = append(sliResults, v1alpha1.SLIResult{
				Name:  sli.Name,
				Class: sli.Class,
				Value: "0", // TODO(m1): real collected value.
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

	if len(sliResults) > 0 {
		values := make([]store.SLIValueRecord, 0, len(sliResults))
		for _, sli := range study.Spec.SLIs {
			values = append(values, store.SLIValueRecord{
				TrialID:     trialID,
				Name:        sli.Name,
				Provider:    sli.Provider,
				Class:       sli.Class,
				Value:       0, // TODO(m1): real collected value.
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

// collectProvider makes a single best-effort provider.Collect call.
func (r *TrialReconciler) collectProvider(ctx context.Context, name string) error {
	conn, err := r.Host.Conn(name)
	if err != nil {
		return fmt.Errorf("provider plugin %q: %w", name, err)
	}
	cctx, cancel := context.WithTimeout(ctx, pluginCallTimeout)
	defer cancel()
	pc := pluginv1.NewProviderClient(conn)
	// TODO(m1): pass Window{t1,t2} + SLIQuery list and parse the response.
	_, err = pc.Collect(cctx, &pluginv1.CollectRequest{})
	return err
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

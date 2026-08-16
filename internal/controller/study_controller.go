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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/aburan28/parallax/api/v1alpha1"
	"github.com/aburan28/parallax/internal/space"
	"github.com/aburan28/parallax/internal/store"
)

// studyLabel keys a Trial to its parent Study; the sweep lists by it to learn what
// has already been materialized and what is still in flight.
const studyLabel = "parallax.dev/study"

// StudyReconciler owns the funnel (DESIGN.md §4.1, §7): it registers the study+run in
// the results DB, materializes Trial CRs, and advances the study through its phases.
type StudyReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Deps
	// Strategy is the strategy-plugin call surface; when nil it is built from
	// Deps.Host. Tests inject a fake.
	Strategy strategyAsker
}

// +kubebuilder:rbac:groups=parallax.dev,resources=studies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=parallax.dev,resources=studies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=parallax.dev,resources=studies/finalizers,verbs=update
// +kubebuilder:rbac:groups=parallax.dev,resources=trials,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile registers the study in the results DB and materializes the first screening
// trial, then advances Pending -> Sweeping. The full sweep/select/validate funnel is
// TODO(m1).
func (r *StudyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var study v1alpha1.Study
	if err := r.Get(ctx, req.NamespacedName, &study); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !study.DeletionTimestamp.IsZero() {
		// Trials are owned; garbage collection removes them. Results-DB rows are retained
		// for offline replay (DESIGN.md §15). TODO(m1): finalize the run row on delete.
		return ctrl.Result{}, nil
	}

	if study.Status.Phase == "" {
		study.Status.Phase = v1alpha1.StudyPhasePending
	}
	study.Status.ObservedGeneration = study.Generation

	// 1. Ensure a results-DB study record (idempotent on spec hash).
	specJSON, err := json.Marshal(study.Spec)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("marshal study spec: %w", err)
	}
	studyRec := &store.StudyRecord{
		Name:      study.Name,
		Namespace: study.Namespace,
		Spec:      specJSON,
		SpecHash:  shortHash(specJSON),
		CreatedAt: time.Now().UTC(),
	}
	studyID, err := r.Store.UpsertStudy(ctx, studyRec)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("upsert study %q: %w", study.Name, err)
	}

	// 2. Ensure a run for this study, recording its ID on status.
	// TODO(m1): CreateRun must be idempotent per (study, seed) so a status-update crash
	// between CreateRun and Status().Update does not leak duplicate runs.
	if study.Status.RunID == "" {
		runRec := &store.RunRecord{
			StudyID:     studyID,
			Seed:        seedFromUID(string(study.UID)),
			Fingerprint: json.RawMessage(`{}`), // TODO(m1): stamp env fingerprint (§9).
			Phase:       string(v1alpha1.StudyPhasePending),
		}
		runID, err := r.Store.CreateRun(ctx, runRec)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("create run for study %q: %w", study.Name, err)
		}
		study.Status.RunID = fmt.Sprintf("%d", runID)
		r.Recorder.Eventf(&study, corev1.EventTypeNormal, "RunCreated", "results-DB run %d created", runID)
	}

	// 3. Materialize screening Trial CRs: the baseline point plus the strategy's next
	//    batch, capped by the trial budget (§7, §8). A strategy that cannot be
	//    resolved fails the study — a sweep that silently searched a different space
	//    than the one it declared is worse than no sweep.
	sweep, err := r.materializeSweep(ctx, &study)
	if err != nil {
		return r.failStudy(ctx, &study, "SweepFailed", err.Error())
	}
	study.Status.TrialsTotal = int32(sweep.total)
	study.Status.TrialsCompleted = int32(sweep.completed)

	// 4. Advance Pending -> Sweeping once the run and first trial exist.
	if study.Status.Phase == v1alpha1.StudyPhasePending {
		study.Status.Phase = v1alpha1.StudyPhaseSweeping
		if runID, ok := parseInt64(study.Status.RunID); ok {
			if err := r.Store.UpdateRunPhase(ctx, runID, string(v1alpha1.StudyPhaseSweeping)); err != nil {
				return ctrl.Result{}, fmt.Errorf("update run phase: %w", err)
			}
		}
		r.Recorder.Event(&study, corev1.EventTypeNormal, "Sweeping", "screening sweep started")
	}

	// The sweep is complete when the strategy says it is done and every trial has
	// reached a terminal phase. Selection is driven offline by `parallax select`
	// until the in-controller funnel lands (TODO(m1)), so the study stays in
	// Sweeping — but the condition makes completion observable.
	switch {
	case sweep.done && sweep.completed >= sweep.total:
		apimeta.SetStatusCondition(&study.Status.Conditions, metav1.Condition{
			Type:               condProgressing,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: study.Generation,
			Reason:             "SweepComplete",
			Message: fmt.Sprintf("screening sweep complete: %d/%d trials collected",
				sweep.completed, sweep.total),
		})
	default:
		apimeta.SetStatusCondition(&study.Status.Conditions, metav1.Condition{
			Type:               condProgressing,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: study.Generation,
			Reason:             "Sweeping",
			Message: fmt.Sprintf("screening sweep in progress: %d/%d trials collected",
				sweep.completed, sweep.total),
		})
	}

	if err := r.Status().Update(ctx, &study); err != nil {
		return ctrl.Result{}, fmt.Errorf("update study status: %w", err)
	}
	log.Info("reconciled study", "phase", study.Status.Phase, "run", study.Status.RunID,
		"trials", sweep.total, "completed", sweep.completed, "sweepDone", sweep.done)
	return ctrl.Result{}, nil
}

// failStudy drives the study to a terminal Failed phase with a recorded reason. Used
// for configuration errors the study cannot recover from on its own — chiefly an
// unresolvable search strategy.
func (r *StudyReconciler) failStudy(ctx context.Context, study *v1alpha1.Study, reason, detail string) (ctrl.Result, error) {
	study.Status.Phase = v1alpha1.StudyPhaseFailed
	apimeta.SetStatusCondition(&study.Status.Conditions, metav1.Condition{
		Type:               condProgressing,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: study.Generation,
		Reason:             reason,
		Message:            detail,
	})
	r.Recorder.Eventf(study, corev1.EventTypeWarning, reason, "%s", detail)
	if err := r.Status().Update(ctx, study); err != nil {
		return ctrl.Result{}, fmt.Errorf("update failed-study status: %w", err)
	}
	return ctrl.Result{}, nil
}

// screeningPoint is one config point to materialize as a screening trial.
type screeningPoint struct {
	configHash  string
	assignments map[string]string // nil for the baseline point
	isBaseline  bool
}

// sweepState is what one materializeSweep pass observed and did.
type sweepState struct {
	// total is every screening trial that exists for the study.
	total int
	// completed is how many of them have reached a terminal phase.
	completed int
	// done reports that the strategy has nothing further to suggest.
	done bool
}

// materializeSweep advances the screening sweep by one batch.
//
// It is a loop, not a one-shot expansion: the strategy is asked for the next points
// only when there is room in flight, so an adaptive strategy sees the previous
// batch's results before choosing the next. That is what makes the strategy seam real
// — a Bayesian or ASHA plugin needs feedback, and the old whole-space expansion could
// never give it any (docs/GENERALIZATION.md G3).
//
// Idempotent: trials are deterministically named by config hash and workload, so
// re-reconciles skip what already exists (DESIGN.md §7, §8).
func (r *StudyReconciler) materializeSweep(ctx context.Context, study *v1alpha1.Study) (sweepState, error) {
	log := logf.FromContext(ctx)

	var existing v1alpha1.TrialList
	if err := r.List(ctx, &existing,
		client.InNamespace(study.Namespace),
		client.MatchingLabels{studyLabel: study.Name},
	); err != nil {
		return sweepState{}, fmt.Errorf("list trials for study %q: %w", study.Name, err)
	}

	state := sweepState{total: len(existing.Items)}
	seen := map[string]bool{}
	pending := map[string]bool{}
	inFlight := 0
	for i := range existing.Items {
		t := &existing.Items[i]
		seen[t.Spec.ConfigHash] = true
		if terminalTrialPhase(t.Status.Phase) {
			state.completed++
		} else {
			inFlight++
			pending[t.Spec.ConfigHash] = true
		}
	}

	// The baseline point always screens; its hash matches space.BaselineHash so
	// `parallax select` can identify the baseline row (§7, §12).
	baseline := space.BaselineHash(study.Spec)
	if !seen[baseline] {
		n, err := r.ensureTrials(ctx, study, screeningPoint{configHash: baseline, isBaseline: true})
		if err != nil {
			return state, err
		}
		seen[baseline] = true
		pending[baseline] = true
		inFlight += n
		state.total += n
	}

	// Budget is counted in distinct config points: reps and workloads of one point are
	// the same experiment (§8 budgets).
	maxPoints := int(study.Spec.Budgets.MaxTrials)
	if maxPoints > 0 && len(seen) >= maxPoints {
		log.V(1).Info("sweep budget reached", "points", len(seen), "maxTrials", maxPoints)
		state.done = true
		return state, nil
	}
	if len(study.Spec.Space.Dimensions) == 0 {
		state.done = true // baseline-only study
		return state, nil
	}

	cfg := parseStrategyConfig(study.Spec.Space.Strategy.Config)
	batch := cfg.BatchSize
	if batch <= 0 {
		batch = defaultAskBatch
	}
	if inFlight >= batch {
		return state, nil // enough in flight; wait for results before asking again
	}
	want := batch - inFlight
	if maxPoints > 0 && len(seen)+want > maxPoints {
		want = maxPoints - len(seen)
	}

	runID, _ := parseInt64(study.Status.RunID)
	observed, err := r.observations(ctx, study, runID)
	if err != nil {
		return state, err
	}
	seed := cfg.Seed
	if seed == 0 {
		seed = seedFromUID(string(study.UID))
	}

	points, done, err := r.askStrategy(ctx, study, askInput{
		seed:     seed,
		count:    want,
		cfg:      cfg,
		rawCfg:   rawConfigBytes(study.Spec.Space.Strategy.Config),
		budget:   study.Spec.Budgets,
		observed: observed,
		pending:  sortedKeys(pending),
		seen:     seen,
	})
	if err != nil {
		return state, err
	}
	state.done = done

	for _, a := range points {
		if maxPoints > 0 && len(seen) >= maxPoints {
			break
		}
		h := space.CanonicalHash(a)
		if seen[h] {
			continue
		}
		n, err := r.ensureTrials(ctx, study, screeningPoint{configHash: h, assignments: a})
		if err != nil {
			return state, err
		}
		seen[h] = true
		state.total += n
	}
	return state, nil
}

// ensureTrials materializes one config point against every workload and returns how
// many trials it created. A config that wins under one load shape and loses under
// another is exactly what the study exists to surface, so every point screens against
// every workload. A study with no workloads still materializes its points.
func (r *StudyReconciler) ensureTrials(ctx context.Context, study *v1alpha1.Study, p screeningPoint) (int, error) {
	if len(study.Spec.Workloads) == 0 {
		if err := r.ensureTrial(ctx, study, p, -1); err != nil {
			return 0, err
		}
		return 1, nil
	}
	for i := range study.Spec.Workloads {
		if err := r.ensureTrial(ctx, study, p, i); err != nil {
			return i, err
		}
	}
	return len(study.Spec.Workloads), nil
}

// terminalTrialPhase reports whether a trial has stopped moving.
func terminalTrialPhase(p v1alpha1.TrialPhase) bool {
	switch p {
	case v1alpha1.TrialPhaseCollected, v1alpha1.TrialPhaseFailed,
		v1alpha1.TrialPhaseAborted, v1alpha1.TrialPhaseInvalid:
		return true
	default:
		return false
	}
}

func parseStrategyConfig(raw *runtime.RawExtension) strategyConfig {
	var c strategyConfig
	if raw != nil && len(raw.Raw) > 0 {
		_ = json.Unmarshal(raw.Raw, &c)
	}
	return c
}

// rawConfigBytes returns a RawExtension's bytes, or nil.
func rawConfigBytes(raw *runtime.RawExtension) []byte {
	if raw == nil {
		return nil
	}
	return raw.Raw
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ensureTrial creates one screening Trial CR for a (config point, workload) pair,
// owned by the study, if it does not already exist. Deterministic naming (by config
// hash and workload index) keeps it idempotent. A workloadIdx below zero means the
// study declares no workloads.
func (r *StudyReconciler) ensureTrial(ctx context.Context, study *v1alpha1.Study, p screeningPoint, workloadIdx int) error {
	var workloadName string
	var fidelity v1alpha1.FidelitySpec
	if workloadIdx >= 0 && workloadIdx < len(study.Spec.Workloads) {
		w := study.Spec.Workloads[workloadIdx]
		workloadName = w.Name
		fidelity = v1alpha1.FidelitySpec{Warmup: w.Warmup, Measure: w.Measure}
	}

	suffix := p.configHash
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	// The workload index (not its name) keeps the name bounded and DNS-safe; the
	// readable name rides on the parallax.dev/workload label.
	trialName := fmt.Sprintf("%s-screen-%s", study.Name, suffix)
	if workloadIdx >= 0 {
		trialName = fmt.Sprintf("%s-screen-w%d-%s", study.Name, workloadIdx, suffix)
	}

	var existing v1alpha1.Trial
	err := r.Get(ctx, client.ObjectKey{Namespace: study.Namespace, Name: trialName}, &existing)
	if err == nil {
		return nil // already materialized
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get screening trial %q: %w", trialName, err)
	}

	labels := map[string]string{
		studyLabel:          study.Name,
		"parallax.dev/mode": string(v1alpha1.TrialModeScreening),
	}
	if workloadName != "" {
		labels["parallax.dev/workload"] = workloadName
	}
	if p.isBaseline {
		labels["parallax.dev/baseline"] = "true"
	}
	trial := &v1alpha1.Trial{
		ObjectMeta: metav1.ObjectMeta{
			Name:      trialName,
			Namespace: study.Namespace,
			Labels:    labels,
		},
		Spec: v1alpha1.TrialSpec{
			StudyRef:   v1alpha1.LocalRef{Name: study.Name},
			ConfigHash: p.configHash,
			Dimensions: p.assignments,
			Workload:   workloadName,
			Fidelity:   fidelity,
			Mode:       v1alpha1.TrialModeScreening,
		},
	}
	switch {
	case p.isBaseline && study.Spec.Baseline.Values != nil:
		trial.Spec.Config = study.Spec.Baseline.Values.DeepCopy()
	case len(p.assignments) > 0:
		if raw, err := json.Marshal(p.assignments); err == nil {
			trial.Spec.Config = &runtime.RawExtension{Raw: raw}
		}
	}
	if err := controllerutil.SetControllerReference(study, trial, r.Scheme); err != nil {
		return fmt.Errorf("set owner ref on trial %q: %w", trialName, err)
	}
	if err := r.Create(ctx, trial); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("create screening trial %q: %w", trialName, err)
	}
	r.Recorder.Eventf(study, corev1.EventTypeNormal, "TrialCreated", "screening trial %q created", trialName)
	return nil
}

// SetupWithManager registers the reconciler, watching Study and the Trials it owns.
func (r *StudyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Study{}).
		Owns(&v1alpha1.Trial{}).
		Named("study").
		Complete(r)
}

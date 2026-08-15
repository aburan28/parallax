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
	"strings"
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

// StudyReconciler owns the funnel (DESIGN.md §4.1, §7): it registers the study+run in
// the results DB, materializes Trial CRs, and advances the study through its phases.
type StudyReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Deps
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

	// 3. Materialize screening Trial CRs: the baseline point plus the strategy's
	//    expansion of the search space, capped by the trial budget (§7, §8).
	total, err := r.materializeSweep(ctx, &study)
	if err != nil {
		return ctrl.Result{}, err
	}
	study.Status.TrialsTotal = int32(total)

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

	apimeta.SetStatusCondition(&study.Status.Conditions, metav1.Condition{
		Type:               condProgressing,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: study.Generation,
		Reason:             "Sweeping",
		Message:            "screening sweep in progress",
	})

	if err := r.Status().Update(ctx, &study); err != nil {
		return ctrl.Result{}, fmt.Errorf("update study status: %w", err)
	}
	log.Info("reconciled study", "phase", study.Status.Phase, "run", study.Status.RunID)
	return ctrl.Result{}, nil
}

// screeningPoint is one config point to materialize as a screening trial.
type screeningPoint struct {
	configHash  string
	assignments map[string]string // nil for the baseline point
	isBaseline  bool
}

// strategyConfig is the small config block carried by grid/random strategies.
type strategyConfig struct {
	Points int   `json:"points"`
	Seed   int64 `json:"seed"`
}

// materializeSweep creates the baseline screening trial plus the strategy's expansion
// of the search space, capped by the trial budget, and returns the total count. It is
// idempotent: trials are deterministically named by config hash, so re-reconciles skip
// existing ones (DESIGN.md §7, §8).
func (r *StudyReconciler) materializeSweep(ctx context.Context, study *v1alpha1.Study) (int, error) {
	// The baseline point always screens; its hash matches space.BaselineHash so
	// `parallax select` can identify the baseline row (§7, §12).
	points := []screeningPoint{{configHash: space.BaselineHash(study.Spec), isBaseline: true}}

	expanded, err := r.expandSweep(study)
	if err != nil {
		return 0, err
	}
	maxTrials := int(study.Spec.Budgets.MaxTrials)
	for _, a := range expanded {
		if maxTrials > 0 && len(points) >= maxTrials {
			break // trial budget reached (§8 budgets)
		}
		points = append(points, screeningPoint{configHash: space.CanonicalHash(a), assignments: a})
	}

	// Every config point screens against every workload: a config that wins under one
	// load shape and loses under another is exactly what the study exists to surface.
	// A study with no workloads still materializes its points (unit/dry-run paths).
	total := 0
	for _, p := range points {
		if len(study.Spec.Workloads) == 0 {
			if err := r.ensureTrial(ctx, study, p, -1); err != nil {
				return 0, err
			}
			total++
			continue
		}
		for i := range study.Spec.Workloads {
			if err := r.ensureTrial(ctx, study, p, i); err != nil {
				return 0, err
			}
			total++
		}
	}
	return total, nil
}

// expandSweep produces the config assignments for the sweep using the study's strategy.
// grid and random are evaluated in-controller (compiled in for --local zero-dep runs,
// §5.1); other strategies fall back to a grid until the strategy-plugin ask/tell loop
// lands (TODO(m1)).
func (r *StudyReconciler) expandSweep(study *v1alpha1.Study) ([]map[string]string, error) {
	if len(study.Spec.Space.Dimensions) == 0 {
		return nil, nil
	}
	sp, err := space.Resolve(study.Spec.Space)
	if err != nil {
		return nil, fmt.Errorf("resolve search space: %w", err)
	}
	strat := strings.ToLower(study.Spec.Space.Strategy.Plugin)
	cfg := parseStrategyConfig(study.Spec.Space.Strategy.Config)
	if strings.Contains(strat, "random") {
		n := cfg.Points
		if n <= 0 {
			n = 20
		}
		seed := cfg.Seed
		if seed == 0 {
			seed = seedFromUID(string(study.UID))
		}
		return randomPoints(sp, n, seed)
	}
	// grid (default), sobol, asha — grid expansion until plugin-driven search lands.
	return space.GridPoints(sp)
}

// randomPoints draws up to n unique random assignments from the space.
func randomPoints(sp *space.Space, n int, seed int64) ([]map[string]string, error) {
	seen := map[string]bool{}
	out := make([]map[string]string, 0, n)
	for i := 0; i < n*8 && len(out) < n; i++ {
		p, err := space.RandomPoint(sp, seed+int64(i))
		if err != nil {
			return nil, err
		}
		h := space.CanonicalHash(p)
		if seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, p)
	}
	return out, nil
}

func parseStrategyConfig(raw *runtime.RawExtension) strategyConfig {
	var c strategyConfig
	if raw != nil && len(raw.Raw) > 0 {
		_ = json.Unmarshal(raw.Raw, &c)
	}
	return c
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
		"parallax.dev/study": study.Name,
		"parallax.dev/mode":  string(v1alpha1.TrialModeScreening),
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

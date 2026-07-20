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
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/aburan28/parallax/api/v1alpha1"
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

	// 3. Materialize a single screening Trial CR for the baseline point (M0). The full
	//    space expansion / strategy ask loop is TODO(m1).
	if err := r.ensureScreeningTrial(ctx, &study); err != nil {
		return ctrl.Result{}, err
	}
	study.Status.TrialsTotal = 1

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

// ensureScreeningTrial creates the baseline screening Trial CR owned by the study, if it
// does not already exist. Deterministic naming keeps it idempotent across reconciles.
func (r *StudyReconciler) ensureScreeningTrial(ctx context.Context, study *v1alpha1.Study) error {
	// Config hash of the baseline point: the baseline values (or its name) fingerprint.
	var baselineRaw []byte
	if study.Spec.Baseline.Values != nil {
		baselineRaw = study.Spec.Baseline.Values.Raw
	}
	if len(baselineRaw) == 0 {
		baselineRaw = []byte(study.Spec.Baseline.Name)
	}
	configHash := shortHash(append([]byte("baseline:"), baselineRaw...))

	var workloadName string
	var fidelity v1alpha1.FidelitySpec
	if len(study.Spec.Workloads) > 0 {
		w := study.Spec.Workloads[0]
		workloadName = w.Name
		fidelity = v1alpha1.FidelitySpec{Warmup: w.Warmup, Measure: w.Measure}
	}

	trialName := fmt.Sprintf("%s-screen-%s", study.Name, configHash[:8])

	var existing v1alpha1.Trial
	err := r.Get(ctx, client.ObjectKey{Namespace: study.Namespace, Name: trialName}, &existing)
	if err == nil {
		return nil // already materialized
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get screening trial %q: %w", trialName, err)
	}

	trial := &v1alpha1.Trial{
		ObjectMeta: metav1.ObjectMeta{
			Name:      trialName,
			Namespace: study.Namespace,
			Labels: map[string]string{
				"parallax.dev/study": study.Name,
				"parallax.dev/mode":  string(v1alpha1.TrialModeScreening),
			},
		},
		Spec: v1alpha1.TrialSpec{
			StudyRef:   v1alpha1.LocalRef{Name: study.Name},
			ConfigHash: configHash,
			Workload:   workloadName,
			Fidelity:   fidelity,
			Mode:       v1alpha1.TrialModeScreening,
		},
	}
	if study.Spec.Baseline.Values != nil {
		trial.Spec.Config = study.Spec.Baseline.Values.DeepCopy()
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

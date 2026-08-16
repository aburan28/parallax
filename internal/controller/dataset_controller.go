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
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/aburan28/parallax/api/v1alpha1"
	"github.com/aburan28/parallax/internal/store"
)

// DatasetReconciler verifies Dataset CRs and stamps the content digest used by readback
// fidelity SLIs (DESIGN.md §4.1, §10). M0 is a skeleton verify.
type DatasetReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
	Deps
}

// +kubebuilder:rbac:groups=parallax.dev,resources=datasets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=parallax.dev,resources=datasets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=parallax.dev,resources=datasets/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// datasetSourceKey renders a source into the stable identity string the placeholder
// digest is derived from. Kinds address their corpus differently — a named in-cluster
// object, a URI, or config alone for a generator — so all three contribute.
func datasetSourceKey(src v1alpha1.DatasetSource) string {
	parts := []string{src.Kind}
	if src.Ref != nil {
		parts = append(parts, src.Ref.Name)
	}
	if src.URI != "" {
		parts = append(parts, src.URI)
	}
	if src.Config != nil {
		parts = append(parts, string(src.Config.Raw))
	}
	return strings.Join(parts, "/")
}

// Reconcile stamps the dataset Verified. TODO(m1): delegate real verification to the
// load driver that consumes the corpus — only it knows the record format — and record
// the true content digest and record count (docs/GENERALIZATION.md G2).
func (r *DatasetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var ds v1alpha1.Dataset
	if err := r.Get(ctx, req.NamespacedName, &ds); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !ds.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	// Idempotent: once verified, do nothing further.
	if ds.Status.Phase == v1alpha1.DatasetPhaseVerified {
		return ctrl.Result{}, nil
	}

	if ds.Status.Phase == "" {
		ds.Status.Phase = v1alpha1.DatasetPhasePending
	}
	ds.Status.ObservedGeneration = ds.Generation

	// TODO(m1): fetch and cross-check the manifest against storage; compute the real
	// content digest and record count. For M0 we derive a placeholder id digest from the
	// dataset identity so downstream fingerprints have a stable value to reference.
	if ds.Status.IDDigest == "" {
		ds.Status.IDDigest = "sha256:" + shortHash([]byte(ds.Namespace+"/"+ds.Name+"/"+datasetSourceKey(ds.Spec.Source)))
	}
	now := metav1.Now()
	ds.Status.VerifiedAt = &now
	ds.Status.Phase = v1alpha1.DatasetPhaseVerified

	apimeta.SetStatusCondition(&ds.Status.Conditions, metav1.Condition{
		Type:               condVerified,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: ds.Generation,
		Reason:             "SkeletonVerify",
		Message:            "M0 skeleton verify; real manifest verification is TODO(m1)",
	})

	// Register the dataset in the results DB so studies can reference it (§15).
	var manifest []byte
	if ds.Status.Manifest != nil {
		manifest = ds.Status.Manifest.Raw
	}
	if err := r.Store.UpsertDataset(ctx, &store.DatasetRecord{
		Name:       ds.Name,
		Manifest:   manifest,
		IDDigest:   ds.Status.IDDigest,
		VerifiedAt: &now.Time,
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("upsert dataset %q: %w", ds.Name, err)
	}

	r.Recorder.Event(&ds, corev1.EventTypeNormal, "Verified", "dataset verified (M0 skeleton)")
	if err := r.Status().Update(ctx, &ds); err != nil {
		return ctrl.Result{}, fmt.Errorf("update dataset status: %w", err)
	}
	log.Info("verified dataset", "digest", ds.Status.IDDigest)
	return ctrl.Result{}, nil
}

// SetupWithManager registers the reconciler for Dataset objects.
func (r *DatasetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.Dataset{}).
		Named("dataset").
		Complete(r)
}

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
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/aburan28/parallax/api/v1alpha1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add scheme: %v", err)
	}
	return s
}

func newStudyReconciler(t *testing.T, objs ...runtime.Object) *StudyReconciler {
	t.Helper()
	s := testScheme(t)
	cl := fake.NewClientBuilder().WithScheme(s).Build()
	return &StudyReconciler{
		Client:   cl,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(128),
	}
}

func gridStudy(name string, maxTrials int32) *v1alpha1.Study {
	return &v1alpha1.Study{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "bench", UID: types.UID("uid-" + name)},
		Spec: v1alpha1.StudySpec{
			Baseline: v1alpha1.BaselineSpec{Name: "defaults"},
			Space: v1alpha1.SpaceSpec{
				Dimensions: []v1alpha1.Dimension{
					{Name: "a", Categorical: &v1alpha1.CategoricalValues{Values: []string{"1", "2"}}},
					{Name: "b", Categorical: &v1alpha1.CategoricalValues{Values: []string{"x", "y", "z"}}},
				},
				Strategy: v1alpha1.PluginRef{Plugin: "strategy-grid"},
			},
			Workloads: []v1alpha1.Workload{{Name: "w0"}},
			Budgets:   v1alpha1.BudgetSpec{MaxTrials: maxTrials},
		},
	}
}

func listTrials(t *testing.T, r *StudyReconciler) []v1alpha1.Trial {
	t.Helper()
	var tl v1alpha1.TrialList
	if err := r.List(context.Background(), &tl); err != nil {
		t.Fatalf("list trials: %v", err)
	}
	return tl.Items
}

// A grid over 2×3 categorical dims yields 6 points; with the baseline that is 7
// screening trials, one of them flagged baseline.
func TestMaterializeSweep_GridExpansion(t *testing.T) {
	r := newStudyReconciler(t)
	study := gridStudy("demo", 0)

	total, err := r.materializeSweep(context.Background(), study)
	if err != nil {
		t.Fatalf("materializeSweep: %v", err)
	}
	if total != 7 { // 1 baseline + 6 grid points
		t.Fatalf("expected 7 trials, got %d", total)
	}

	trials := listTrials(t, r)
	if len(trials) != 7 {
		t.Fatalf("expected 7 Trial CRs created, got %d", len(trials))
	}
	baselines, screening := 0, 0
	for _, tr := range trials {
		if tr.Labels["parallax.dev/baseline"] == "true" {
			baselines++
		}
		if tr.Spec.Mode == v1alpha1.TrialModeScreening {
			screening++
		}
		if tr.Spec.ConfigHash == "" {
			t.Fatalf("trial %q has empty config hash", tr.Name)
		}
	}
	if baselines != 1 {
		t.Fatalf("expected exactly 1 baseline trial, got %d", baselines)
	}
	if screening != 7 {
		t.Fatalf("expected all 7 trials in screening mode, got %d", screening)
	}
}

// The trial budget caps how many points are materialized (baseline included).
func TestMaterializeSweep_BudgetCap(t *testing.T) {
	r := newStudyReconciler(t)
	study := gridStudy("capped", 3)

	total, err := r.materializeSweep(context.Background(), study)
	if err != nil {
		t.Fatalf("materializeSweep: %v", err)
	}
	if total != 3 {
		t.Fatalf("budget maxTrials=3 must cap at 3, got %d", total)
	}
	if got := len(listTrials(t, r)); got != 3 {
		t.Fatalf("expected 3 Trial CRs, got %d", got)
	}
}

// Re-running the sweep must not create duplicate trials (idempotency).
func TestMaterializeSweep_Idempotent(t *testing.T) {
	r := newStudyReconciler(t)
	study := gridStudy("idem", 0)

	if _, err := r.materializeSweep(context.Background(), study); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	first := len(listTrials(t, r))
	if _, err := r.materializeSweep(context.Background(), study); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	second := len(listTrials(t, r))
	if first != second {
		t.Fatalf("sweep not idempotent: %d trials then %d", first, second)
	}
}

// Random strategy honors its point count and is deterministic in trial identity.
func TestExpandSweep_RandomPointCount(t *testing.T) {
	r := newStudyReconciler(t)
	study := &v1alpha1.Study{
		ObjectMeta: metav1.ObjectMeta{Name: "rnd", Namespace: "bench", UID: "uid-rnd"},
		Spec: v1alpha1.StudySpec{
			Space: v1alpha1.SpaceSpec{
				Dimensions: []v1alpha1.Dimension{
					{Name: "n", Int: &v1alpha1.IntRange{Min: 1, Max: 1000}},
				},
				Strategy: v1alpha1.PluginRef{
					Plugin: "strategy-random",
					Config: &runtime.RawExtension{Raw: []byte(`{"points":5,"seed":7}`)},
				},
			},
		},
	}
	pts, err := r.expandSweep(study)
	if err != nil {
		t.Fatalf("expandSweep: %v", err)
	}
	if len(pts) != 5 {
		t.Fatalf("expected 5 random points, got %d", len(pts))
	}
}

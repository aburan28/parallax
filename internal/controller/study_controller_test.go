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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/aburan28/parallax/api/v1alpha1"
	"github.com/aburan28/parallax/internal/store"
	pluginv1 "github.com/aburan28/parallax/proto/plugin/v1"
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
				Strategy: v1alpha1.PluginRef{Plugin: BuiltinGrid},
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

	sweep, err := r.materializeSweep(context.Background(), study)
	if err != nil {
		t.Fatalf("materializeSweep: %v", err)
	}
	if sweep.total != 7 { // 1 baseline + 6 grid points
		t.Fatalf("expected 7 trials, got %d", sweep.total)
	}
	if !sweep.done {
		t.Fatalf("a fully-walked grid must report done")
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

	sweep, err := r.materializeSweep(context.Background(), study)
	if err != nil {
		t.Fatalf("materializeSweep: %v", err)
	}
	if sweep.total != 3 {
		t.Fatalf("budget maxTrials=3 must cap at 3, got %d", sweep.total)
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

// The builtin random strategy honors its point budget across repeated sweeps.
func TestSweep_BuiltinRandomPointBudget(t *testing.T) {
	r := newStudyReconciler(t)
	study := &v1alpha1.Study{
		ObjectMeta: metav1.ObjectMeta{Name: "rnd", Namespace: "bench", UID: "uid-rnd"},
		Spec: v1alpha1.StudySpec{
			Space: v1alpha1.SpaceSpec{
				Dimensions: []v1alpha1.Dimension{
					{Name: "n", Int: &v1alpha1.IntRange{Min: 1, Max: 1000}},
				},
				Strategy: v1alpha1.PluginRef{
					Plugin: BuiltinRandom,
					Config: &runtime.RawExtension{Raw: []byte(`{"points":5,"seed":7,"batchSize":32}`)},
				},
			},
			Workloads: []v1alpha1.Workload{{Name: "w0"}},
		},
	}
	if _, err := r.materializeSweep(context.Background(), study); err != nil {
		t.Fatalf("materializeSweep: %v", err)
	}
	// points counts every distinct config point, baseline included — the same rule
	// budgets.maxTrials uses.
	if got := len(listTrials(t, r)); got != 5 {
		t.Fatalf("expected 5 config points (baseline included), got %d trials", got)
	}
	// A second pass must not exceed the point budget.
	if _, err := r.materializeSweep(context.Background(), study); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if got := len(listTrials(t, r)); got != 5 {
		t.Fatalf("point budget exceeded on resweep: %d trials", got)
	}
}

// fakeStrategy records what the controller asked and replies with canned points.
type fakeStrategy struct {
	suggestions []map[string]string
	done        bool
	err         error
	lastReq     *pluginv1.AskRequest
	lastName    string
	calls       int
}

func (f *fakeStrategy) Ask(_ context.Context, name string, req *pluginv1.AskRequest) (*pluginv1.AskResponse, error) {
	f.calls++
	f.lastName = name
	f.lastReq = req
	if f.err != nil {
		return nil, f.err
	}
	resp := &pluginv1.AskResponse{Done: f.done}
	for _, a := range f.suggestions {
		resp.Suggestions = append(resp.Suggestions, &pluginv1.Suggestion{Assignments: a})
	}
	return resp, nil
}

func pluginStrategyStudy(name string, cfg string) *v1alpha1.Study {
	st := gridStudy(name, 0)
	st.Spec.Space.Strategy = v1alpha1.PluginRef{Plugin: "strategy-bayes"}
	if cfg != "" {
		st.Spec.Space.Strategy.Config = &runtime.RawExtension{Raw: []byte(cfg)}
	}
	return st
}

// A named strategy plugin is actually dialed, and its suggestions become trials.
func TestSweep_StrategyPluginIsCalled(t *testing.T) {
	fake := &fakeStrategy{suggestions: []map[string]string{{"a": "1", "b": "x"}, {"a": "2", "b": "y"}}}
	r := newStudyReconciler(t)
	r.Strategy = fake
	study := pluginStrategyStudy("bayes", "")

	sweep, err := r.materializeSweep(context.Background(), study)
	if err != nil {
		t.Fatalf("materializeSweep: %v", err)
	}
	if fake.calls != 1 {
		t.Fatalf("expected the strategy plugin to be asked once, got %d calls", fake.calls)
	}
	if fake.lastName != "strategy-bayes" {
		t.Fatalf("expected plugin %q to be dialed, got %q", "strategy-bayes", fake.lastName)
	}
	// baseline + the plugin's two suggestions.
	if sweep.total != 3 {
		t.Fatalf("expected 3 trials, got %d", sweep.total)
	}
	// The baseline is already in flight, so the plugin must be told about it.
	if len(fake.lastReq.GetPending()) != 1 {
		t.Fatalf("expected the in-flight baseline in pending, got %v", fake.lastReq.GetPending())
	}
}

// An unknown builtin must fail the study, not silently fall back to a grid. This is
// the regression that matters: the old sweep substring-matched plugin names, so any
// unrecognised strategy quietly became a grid search.
func TestSweep_UnknownBuiltinFailsStudy(t *testing.T) {
	r := newStudyReconciler(t)
	study := gridStudy("bogus", 0)
	study.Spec.Space.Strategy = v1alpha1.PluginRef{Plugin: "builtin:definitely-not-a-strategy"}

	if _, err := r.materializeSweep(context.Background(), study); err == nil {
		t.Fatalf("an unknown builtin strategy must be an error, not a grid fallback")
	}
	if got := len(listTrials(t, r)); got > 1 {
		t.Fatalf("no search points may be materialized for an unresolvable strategy, got %d trials", got)
	}
}

// A strategy name that is neither a builtin nor a reachable plugin fails the study
// through Reconcile, with the reason recorded on status.
func TestReconcile_UnresolvableStrategyMarksStudyFailed(t *testing.T) {
	s := testScheme(t)
	study := gridStudy("unreachable", 0)
	study.Spec.Space.Strategy = v1alpha1.PluginRef{Plugin: "strategy-nowhere"}
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(study).
		WithStatusSubresource(&v1alpha1.Study{}, &v1alpha1.Trial{}).Build()
	st := sqliteStore(t)
	r := &StudyReconciler{
		Client: cl, Scheme: s, Recorder: record.NewFakeRecorder(128),
		Deps: Deps{Store: st},
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "bench", Name: "unreachable"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got v1alpha1.Study
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "bench", Name: "unreachable"}, &got); err != nil {
		t.Fatalf("get study: %v", err)
	}
	if got.Status.Phase != v1alpha1.StudyPhaseFailed {
		t.Fatalf("expected the study to fail on an unresolvable strategy, got phase %q", got.Status.Phase)
	}
}

// The sweep asks in batches and stops while a batch is in flight, so an adaptive
// strategy sees results before choosing the next points.
func TestSweep_BatchesAndWaitsForResults(t *testing.T) {
	fake := &fakeStrategy{suggestions: []map[string]string{{"a": "1", "b": "x"}}}
	r := newStudyReconciler(t)
	r.Strategy = fake
	study := pluginStrategyStudy("batched", `{"batchSize":2}`)

	// Pass 1: baseline (1 in flight) leaves room for 1 more.
	if _, err := r.materializeSweep(context.Background(), study); err != nil {
		t.Fatalf("first sweep: %v", err)
	}
	if got := fake.lastReq.GetCount(); got != 1 {
		t.Fatalf("expected a request for 1 point (batchSize 2 minus the in-flight baseline), got %d", got)
	}
	if got := len(listTrials(t, r)); got != 2 {
		t.Fatalf("expected 2 trials after the first batch, got %d", got)
	}

	// Pass 2: both trials are still in flight, so the strategy must not be asked again.
	before := fake.calls
	if _, err := r.materializeSweep(context.Background(), study); err != nil {
		t.Fatalf("second sweep: %v", err)
	}
	if fake.calls != before {
		t.Fatalf("strategy asked while a full batch was in flight (%d -> %d calls)", before, fake.calls)
	}
}

// Observations handed to a strategy come from the results DB, with the objective
// normalized so a strategy always minimizes.
func TestObservations_NormalizeObjectiveToMinimize(t *testing.T) {
	ctx := context.Background()
	st := sqliteStore(t)
	sid, err := st.UpsertStudy(ctx, &store.StudyRecord{Name: "s", Namespace: "bench", Spec: []byte(`{}`), SpecHash: "h"})
	if err != nil {
		t.Fatalf("seed study: %v", err)
	}
	runID, err := st.CreateRun(ctx, &store.RunRecord{StudyID: sid, Phase: "Sweeping"})
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
	tid, err := st.UpsertTrial(ctx, &store.TrialRecord{
		RunID: runID, ConfigHash: "cfg1", Validity: "valid",
		Config: []byte(`{"a":"1"}`),
	})
	if err != nil {
		t.Fatalf("seed trial: %v", err)
	}
	if err := st.InsertSLIValues(ctx, []store.SLIValueRecord{
		{TrialID: tid, Name: "throughput", Value: 900},
		{TrialID: tid, Name: "cost", Value: 3},
	}); err != nil {
		t.Fatalf("seed sli values: %v", err)
	}

	r := newStudyReconciler(t)
	r.Deps = Deps{Store: st}
	study := &v1alpha1.Study{
		Spec: v1alpha1.StudySpec{Objectives: v1alpha1.ObjectivesSpec{
			Primary: v1alpha1.Objective{SLI: "throughput", Direction: "maximize"},
		}},
	}

	obs, err := r.observations(ctx, study, runID)
	if err != nil {
		t.Fatalf("observations: %v", err)
	}
	if len(obs) != 1 {
		t.Fatalf("expected 1 observation, got %d", len(obs))
	}
	if obs[0].GetObjective() != -900 {
		t.Fatalf("a maximize objective must be negated for the strategy, got %v", obs[0].GetObjective())
	}
	if obs[0].GetAssignments()["a"] != "1" {
		t.Fatalf("observation lost its assignments: %v", obs[0].GetAssignments())
	}
	if !obs[0].GetFeasible() {
		t.Fatalf("a valid trial must be reported feasible")
	}
}

// A trial whose objective SLI was never collected is not an observation: telling a
// strategy that a failed trial scored zero would poison the search.
func TestObservations_SkipsTrialsWithoutTheObjective(t *testing.T) {
	ctx := context.Background()
	st := sqliteStore(t)
	sid, _ := st.UpsertStudy(ctx, &store.StudyRecord{Name: "s", Namespace: "bench", Spec: []byte(`{}`), SpecHash: "h"})
	runID, _ := st.CreateRun(ctx, &store.RunRecord{StudyID: sid, Phase: "Sweeping"})
	tid, err := st.UpsertTrial(ctx, &store.TrialRecord{RunID: runID, ConfigHash: "cfg1", Validity: "valid"})
	if err != nil {
		t.Fatalf("seed trial: %v", err)
	}
	if err := st.InsertSLIValues(ctx, []store.SLIValueRecord{{TrialID: tid, Name: "unrelated", Value: 1}}); err != nil {
		t.Fatalf("seed sli values: %v", err)
	}

	r := newStudyReconciler(t)
	r.Deps = Deps{Store: st}
	study := &v1alpha1.Study{
		Spec: v1alpha1.StudySpec{Objectives: v1alpha1.ObjectivesSpec{
			Primary: v1alpha1.Objective{SLI: "latency", Direction: "minimize"},
		}},
	}

	obs, err := r.observations(ctx, study, runID)
	if err != nil {
		t.Fatalf("observations: %v", err)
	}
	if len(obs) != 0 {
		t.Fatalf("expected no observations when the objective was never collected, got %d", len(obs))
	}
}

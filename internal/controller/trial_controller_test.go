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
	"path/filepath"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/aburan28/parallax/api/v1alpha1"
	"github.com/aburan28/parallax/internal/store"
	_ "github.com/aburan28/parallax/internal/store/sqlite"
	pluginv1 "github.com/aburan28/parallax/proto/plugin/v1"
)

// fakePlugins is an in-memory trialPlugins for driving the reconciler without real
// subprocesses. Nil response fields default to success.
type fakePlugins struct {
	applyResp   *pluginv1.ApplyResponse
	readyResp   *pluginv1.ReadyResponse
	stopResp    *pluginv1.StopResponse
	collectFunc func(provider string, req *pluginv1.CollectRequest) (*pluginv1.CollectResponse, error)
}

func (f fakePlugins) Apply(_ context.Context, _ string, _ *pluginv1.ApplyRequest) (*pluginv1.ApplyResponse, error) {
	if f.applyResp != nil {
		return f.applyResp, nil
	}
	return &pluginv1.ApplyResponse{Applied: true}, nil
}

func (f fakePlugins) Ready(_ context.Context, _ string, _ *pluginv1.ReadyRequest) (*pluginv1.ReadyResponse, error) {
	if f.readyResp != nil {
		return f.readyResp, nil
	}
	return &pluginv1.ReadyResponse{Ready: true}, nil
}

func (f fakePlugins) StartLoad(_ context.Context, _ string, _ *pluginv1.StartRequest) (*pluginv1.StartResponse, error) {
	return &pluginv1.StartResponse{RunRef: "run-1"}, nil
}

func (f fakePlugins) StopLoad(_ context.Context, _ string, _ *pluginv1.StopRequest) (*pluginv1.StopResponse, error) {
	if f.stopResp != nil {
		return f.stopResp, nil
	}
	return &pluginv1.StopResponse{Ok: true}, nil
}

func (f fakePlugins) Collect(_ context.Context, provider string, req *pluginv1.CollectRequest) (*pluginv1.CollectResponse, error) {
	if f.collectFunc != nil {
		return f.collectFunc(provider, req)
	}
	return &pluginv1.CollectResponse{}, nil
}

func sqliteStore(t *testing.T) store.Store {
	t.Helper()
	dsn := "sqlite://" + filepath.Join(t.TempDir(), "results.db")
	st, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// collectAndPersist must route each provider-backed SLI to its provider's Collect and
// persist the real returned values (not placeholder zeros) to the results DB.
func TestCollectAndPersist_RealValuesPersisted(t *testing.T) {
	ctx := context.Background()
	st := sqliteStore(t)
	sid, err := st.UpsertStudy(ctx, &store.StudyRecord{Name: "s", Namespace: "bench", Spec: []byte(`{}`), SpecHash: "h"})
	if err != nil {
		t.Fatalf("seed study: %v", err)
	}
	rid, err := st.CreateRun(ctx, &store.RunRecord{StudyID: sid, Phase: "Sweeping"})
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}

	study := &v1alpha1.Study{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "bench"},
		Spec: v1alpha1.StudySpec{
			SLIs: []v1alpha1.SLISpec{
				{Name: "cpu", Provider: "prometheus", Class: "server", Query: "rate(cpu)"},
				{Name: "loss", Provider: "prometheus", Class: "fidelity"},
			},
		},
		Status: v1alpha1.StudyStatus{RunID: fmt.Sprintf("%d", rid)},
	}
	trial := &v1alpha1.Trial{
		ObjectMeta: metav1.ObjectMeta{Name: "s-screen-abcd", Namespace: "bench", UID: "u1"},
		Spec:       v1alpha1.TrialSpec{ConfigHash: "cfg1", Workload: "w0", Mode: v1alpha1.TrialModeScreening},
	}
	tm := metav1.Now()
	trial.Status.Timeline.T1 = &tm
	trial.Status.Timeline.T2 = &tm

	r := &TrialReconciler{
		Recorder: record.NewFakeRecorder(64),
		Plugins: fakePlugins{collectFunc: func(provider string, req *pluginv1.CollectRequest) (*pluginv1.CollectResponse, error) {
			// The provider must be asked for exactly the two prometheus SLIs.
			if provider != "prometheus" {
				t.Fatalf("unexpected provider %q", provider)
			}
			if len(req.GetQueries()) != 2 {
				t.Fatalf("expected 2 queries, got %d", len(req.GetQueries()))
			}
			if req.GetWindow().GetT1Rfc3339() == "" || req.GetWindow().GetT2Rfc3339() == "" {
				t.Fatalf("collect window must carry t1/t2")
			}
			return &pluginv1.CollectResponse{Values: []*pluginv1.SLIValue{
				{Name: "cpu", Value: 2.5, Ok: true},
				{Name: "loss", Value: 0, Ok: true},
			}}, nil
		}},
	}
	r.Deps = Deps{Store: st}

	if err := r.collectAndPersist(ctx, resolvedPlugins{providers: []string{"prometheus"}}, study, trial); err != nil {
		t.Fatalf("collectAndPersist: %v", err)
	}

	// Status reflects the real collected value.
	got := map[string]string{}
	for _, s := range trial.Status.SLIs {
		got[s.Name] = s.Value
	}
	if got["cpu"] != "2.5" {
		t.Fatalf("expected status cpu=2.5, got %q", got["cpu"])
	}

	// The results DB carries the real value on the narrow fact table.
	vals, err := st.ListSLIValuesForRun(ctx, rid)
	if err != nil {
		t.Fatalf("list sli values: %v", err)
	}
	dbVals := map[string]float64{}
	for _, v := range vals {
		dbVals[v.Name] = v.Value
	}
	if dbVals["cpu"] != 2.5 {
		t.Fatalf("expected DB cpu=2.5, got %v (all=%v)", dbVals["cpu"], dbVals)
	}
	if _, ok := dbVals["loss"]; !ok {
		t.Fatalf("loss SLI not persisted: %v", dbVals)
	}
}

// A reachable target that reports Apply failure must fail the trial.
func TestReconcile_ApplyFailureFailsTrial(t *testing.T) {
	ctx := context.Background()
	s := testScheme(t)
	study := &v1alpha1.Study{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "bench"},
		Spec:       v1alpha1.StudySpec{Target: v1alpha1.PluginRef{Plugin: "target-x"}},
	}
	trial := &v1alpha1.Trial{
		ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: "bench", UID: types.UID("u1")},
		Spec:       v1alpha1.TrialSpec{StudyRef: v1alpha1.LocalRef{Name: "s"}, ConfigHash: "c1"},
		Status:     v1alpha1.TrialStatus{Phase: v1alpha1.TrialPhaseConfiguring},
	}
	cl := fake.NewClientBuilder().WithScheme(s).
		WithObjects(study, trial).
		WithStatusSubresource(&v1alpha1.Trial{}, &v1alpha1.Study{}).
		Build()
	r := &TrialReconciler{
		Client:   cl,
		Scheme:   s,
		Recorder: record.NewFakeRecorder(64),
		Plugins:  fakePlugins{applyResp: &pluginv1.ApplyResponse{Applied: false, Detail: "chart render failed"}},
	}

	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "bench", Name: "t1"}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got v1alpha1.Trial
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "bench", Name: "t1"}, &got); err != nil {
		t.Fatalf("get trial: %v", err)
	}
	if got.Status.Phase != v1alpha1.TrialPhaseFailed {
		t.Fatalf("expected trial Failed after Apply failure, got %q", got.Status.Phase)
	}
}

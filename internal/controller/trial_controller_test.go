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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/aburan28/parallax/api/v1alpha1"
	"github.com/aburan28/parallax/internal/store"
	_ "github.com/aburan28/parallax/internal/store/sqlite"
	pluginv1 "github.com/aburan28/parallax/proto/plugin/v1"
)

// fakePlugins is an in-memory trialPlugins for driving the reconciler without real
// subprocesses. Nil response fields default to success.
type fakePlugins struct {
	applyResp    *pluginv1.ApplyResponse
	readyResp    *pluginv1.ReadyResponse
	stopResp     *pluginv1.StopResponse
	progressResp *pluginv1.ProgressResponse
	collectFunc  func(provider string, req *pluginv1.CollectRequest) (*pluginv1.CollectResponse, error)
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

func (f fakePlugins) Progress(_ context.Context, _ string, _ *pluginv1.ProgressRequest) (*pluginv1.ProgressResponse, error) {
	if f.progressResp != nil {
		return f.progressResp, nil
	}
	return &pluginv1.ProgressResponse{Phase: "Running"}, nil
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

// promSLI builds a prometheus-backed SLI source carrying that provider's own query
// dialect — the core never sees a "query" field of its own.
func promSLI(query string) *v1alpha1.PluginRef {
	return &v1alpha1.PluginRef{
		Plugin: "prometheus",
		Config: &runtime.RawExtension{Raw: []byte(fmt.Sprintf(`{"query":%q}`, query))},
	}
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
				{Name: "cpu", Class: "server", From: promSLI("rate(cpu)")},
				{Name: "loss", Class: "fidelity", From: promSLI("")},
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

// measuringTrial builds a study/trial pair parked in Measuring, with the given
// workload, so the window-closing rules can be driven directly.
func measuringTrial(t *testing.T, wl v1alpha1.Workload, plugins trialPlugins) (*TrialReconciler, client.Client, *v1alpha1.Trial) {
	t.Helper()
	s := testScheme(t)
	study := &v1alpha1.Study{
		ObjectMeta: metav1.ObjectMeta{Name: "s", Namespace: "bench"},
		Spec:       v1alpha1.StudySpec{Workloads: []v1alpha1.Workload{wl}},
	}
	t1 := metav1.Now()
	trial := &v1alpha1.Trial{
		ObjectMeta: metav1.ObjectMeta{Name: "t1", Namespace: "bench", UID: types.UID("u1")},
		Spec: v1alpha1.TrialSpec{
			StudyRef: v1alpha1.LocalRef{Name: "s"}, ConfigHash: "c1", Workload: wl.Name,
		},
		Status: v1alpha1.TrialStatus{
			Phase:    v1alpha1.TrialPhaseMeasuring,
			Timeline: v1alpha1.TrialTimeline{T1: &t1},
			Load:     v1alpha1.LoadRunStatus{RunRef: "run-1"},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(s).WithObjects(study, trial).
		WithStatusSubresource(&v1alpha1.Trial{}, &v1alpha1.Study{}).Build()
	r := &TrialReconciler{Client: cl, Scheme: s, Recorder: record.NewFakeRecorder(64), Plugins: plugins}
	return r, cl, trial
}

func reconcileTrial(t *testing.T, r *TrialReconciler, cl client.Client) v1alpha1.Trial {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "bench", Name: "t1"},
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var got v1alpha1.Trial
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "bench", Name: "t1"}, &got); err != nil {
		t.Fatalf("get trial: %v", err)
	}
	return got
}

// A duration-completed workload must hold the measurement window open for its full
// measure duration rather than closing on the next reconcile tick.
func TestMeasuring_HoldsWindowForMeasureDuration(t *testing.T) {
	r, cl, _ := measuringTrial(t, v1alpha1.Workload{
		Name:       "w0",
		Driver:     v1alpha1.PluginRef{Plugin: "driver-x"},
		Completion: v1alpha1.CompletionDuration,
		Measure:    metav1.Duration{Duration: time.Hour},
	}, fakePlugins{})

	got := reconcileTrial(t, r, cl)
	if got.Status.Phase != v1alpha1.TrialPhaseMeasuring {
		t.Fatalf("expected trial to stay Measuring inside the window, got %q", got.Status.Phase)
	}
	if got.Status.Timeline.T2 != nil {
		t.Fatalf("t2 stamped before the measurement window elapsed")
	}
}

// A driver-completed workload ignores the measure duration: the window closes when
// the driver reports done, and not before.
func TestMeasuring_DriverCompletionClosesWindow(t *testing.T) {
	wl := v1alpha1.Workload{
		Name:       "batch",
		Driver:     v1alpha1.PluginRef{Plugin: "driver-x"},
		Completion: v1alpha1.CompletionDriver,
		Measure:    metav1.Duration{Duration: time.Nanosecond}, // would close instantly if honored
	}

	// Still running: the elapsed measure duration must not close the window.
	r, cl, _ := measuringTrial(t, wl, fakePlugins{progressResp: &pluginv1.ProgressResponse{Phase: "Running"}})
	if got := reconcileTrial(t, r, cl); got.Status.Phase != v1alpha1.TrialPhaseMeasuring {
		t.Fatalf("expected Measuring while the driver is still running, got %q", got.Status.Phase)
	}

	// Done: the window closes and the driver's summary metrics land on status.
	r, cl, _ = measuringTrial(t, wl, fakePlugins{
		progressResp: &pluginv1.ProgressResponse{Phase: "Completed", Done: true},
		stopResp:     &pluginv1.StopResponse{Ok: true, Metrics: map[string]float64{"records_processed": 4200}},
	})
	got := reconcileTrial(t, r, cl)
	if got.Status.Phase != v1alpha1.TrialPhaseDraining {
		t.Fatalf("expected Draining once the driver reported done, got %q", got.Status.Phase)
	}
	if got.Status.Timeline.T2 == nil {
		t.Fatalf("t2 not stamped when the window closed")
	}
	if got.Status.Load.Metrics["records_processed"] != "4200" {
		t.Fatalf("driver metrics not recorded on status: %v", got.Status.Load.Metrics)
	}
}

// A driver that reports its abort policy fired must terminate the trial as Aborted —
// the abort policy is enforced by the core, not merely declared in the CRD.
func TestMeasuring_DriverAbortEndsTrial(t *testing.T) {
	r, cl, _ := measuringTrial(t, v1alpha1.Workload{
		Name:       "w0",
		Driver:     v1alpha1.PluginRef{Plugin: "driver-x"},
		Completion: v1alpha1.CompletionDuration,
		Measure:    metav1.Duration{Duration: time.Hour},
	}, fakePlugins{progressResp: &pluginv1.ProgressResponse{Aborted: true, AbortReason: "error rate 12% > 5%"}})

	got := reconcileTrial(t, r, cl)
	if got.Status.Phase != v1alpha1.TrialPhaseAborted {
		t.Fatalf("expected Aborted after the driver aborted, got %q", got.Status.Phase)
	}
	if got.Status.Load.AbortReason == "" {
		t.Fatalf("abort reason not recorded")
	}
}

// A workload whose driver never completes must abort at maxDuration rather than
// holding the window open forever.
func TestMeasuring_MaxDurationAborts(t *testing.T) {
	r, cl, _ := measuringTrial(t, v1alpha1.Workload{
		Name:        "batch",
		Driver:      v1alpha1.PluginRef{Plugin: "driver-x"},
		Completion:  v1alpha1.CompletionDriver,
		MaxDuration: metav1.Duration{Duration: time.Nanosecond},
	}, fakePlugins{progressResp: &pluginv1.ProgressResponse{Phase: "Running"}})

	got := reconcileTrial(t, r, cl)
	if got.Status.Phase != v1alpha1.TrialPhaseAborted {
		t.Fatalf("expected Aborted at maxDuration, got %q", got.Status.Phase)
	}
}

// An SLI sourced from the load driver reads the driver's own metric names, with no
// provider plugin involved — the path that makes non-HTTP drivers usable.
func TestCollectSLIs_DriverMetricSource(t *testing.T) {
	study := &v1alpha1.Study{
		Spec: v1alpha1.StudySpec{SLIs: []v1alpha1.SLISpec{
			{Name: "throughput", Class: "client", Driver: &v1alpha1.DriverMetric{Metric: "records_per_second"}},
			{Name: "missing", Class: "client", Driver: &v1alpha1.DriverMetric{Metric: "not_reported"}},
		}},
	}
	trial := &v1alpha1.Trial{
		Status: v1alpha1.TrialStatus{
			Load: v1alpha1.LoadRunStatus{Metrics: map[string]string{"records_per_second": "1234.5"}},
		},
	}
	r := &TrialReconciler{Recorder: record.NewFakeRecorder(64), Plugins: fakePlugins{}}

	got := r.collectSLIs(context.Background(), study, trial)
	if !got["throughput"].ok || got["throughput"].value != 1234.5 {
		t.Fatalf("expected throughput=1234.5 from driver metrics, got %+v", got["throughput"])
	}
	if got["missing"].ok {
		t.Fatalf("an unreported driver metric must not resolve as ok: %+v", got["missing"])
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

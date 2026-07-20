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

// Command loaddriver-kapture is a first-party parallax LoadDriver plugin (DESIGN.md
// §5.1, App. A.2). It composes a kapture CaptureLoadTest, fans it out into replay
// shards across the spokes of selected cells, watches the per-cell rollup, and
// aggregates the shard run reports on Stop.
//
// M0 is a skeleton: Plan derives the shard count from the workload, Start synthesizes
// a run reference, Watch streams a few synthetic progress events then a terminal one,
// and Stop returns a zeroed RunReport. TODO(m1) markers flag where the real
// CaptureLoadTest lifecycle will land.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/aburan28/parallax/pkg/plugin"
	pluginv1 "github.com/aburan28/parallax/proto/plugin/v1"
)

// logger writes to stderr only; stdout is reserved for the SDK handshake line.
var logger = log.New(os.Stderr, "loaddriver-kapture: ", log.LstdFlags|log.Lmsgprefix)

// watchStepInterval paces the synthetic progress stream so Watch behaves like a real
// streaming RPC. It is honored against the stream context so cancellation is prompt.
const watchStepInterval = 250 * time.Millisecond

// workload mirrors the CaptureLoadTest.spec fields parallax passes through (DESIGN.md
// §8, App. A.2). Only the fields M0 needs to plan the run are modeled.
type workload struct {
	Engine       string `json:"engine"`
	Distribution struct {
		Cells                []string `json:"cells"`
		MaxSpokes            int64    `json:"maxSpokes"`
		WorkersPerSpoke      int64    `json:"workersPerSpoke"`
		ConcurrencyPerWorker int64    `json:"concurrencyPerWorker"`
		Presharded           bool     `json:"presharded"`
	} `json:"distribution"`
	Rate struct {
		Mode              string  `json:"mode"`
		RequestsPerSecond float64 `json:"requestsPerSecond"`
	} `json:"rate"`
}

// parseWorkload decodes the workload JSON. An empty body yields a zero workload,
// which plans a single shard.
func parseWorkload(raw []byte) (*workload, error) {
	var w workload
	if len(raw) == 0 {
		return &w, nil
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("decode workload: %w", err)
	}
	return &w, nil
}

// totalShards derives the number of replay shards from the distribution: one shard
// per worker per spoke. Missing/zero values default to a single shard.
func (w *workload) totalShards() int64 {
	spokes := w.Distribution.MaxSpokes
	if spokes <= 0 {
		spokes = 1
	}
	workers := w.Distribution.WorkersPerSpoke
	if workers <= 0 {
		workers = 1
	}
	return spokes * workers
}

func (w *workload) engineOrDefault() string {
	if w.Engine == "" {
		return "builtin"
	}
	return w.Engine
}

// server implements the LoadDriver service for kapture.
type server struct {
	pluginv1.UnimplementedLoadDriverServer
}

// Plan validates the workload and reports how many replay shards it fans out to.
func (s *server) Plan(_ context.Context, req *pluginv1.PlanRequest) (*pluginv1.PlanResponse, error) {
	w, err := parseWorkload(req.GetWorkloadJson())
	if err != nil {
		return &pluginv1.PlanResponse{Ok: false, Detail: fmt.Sprintf("invalid workload: %v", err)}, nil
	}
	shards := w.totalShards()
	return &pluginv1.PlanResponse{
		Ok:          true,
		Detail:      fmt.Sprintf("loaddriver-kapture: planned %d replay shard(s) with engine %q", shards, w.engineOrDefault()),
		TotalShards: shards,
	}, nil
}

// Start begins a load run and returns an opaque handle for Watch/Stop.
func (s *server) Start(_ context.Context, req *pluginv1.StartRequest) (*pluginv1.StartResponse, error) {
	// TODO(m1): compose and create a CaptureLoadTest CR for this trial (dataset, rate,
	// distribution, abort policy) and return its namespace/name as the run_ref.
	trialID := req.GetTrialId()
	if trialID == "" {
		trialID = "trial"
	}
	runRef := fmt.Sprintf("parallax-system/clt-%s", trialID)
	logger.Printf("start run %s", runRef)
	return &pluginv1.StartResponse{
		RunRef: runRef,
		Detail: "loaddriver-kapture: run reference synthesized (no CaptureLoadTest created in M0)",
	}, nil
}

// Watch streams progress until the run terminates. In M0 it emits a few synthetic
// progress events followed by a terminal event.
func (s *server) Watch(req *pluginv1.WatchRequest, stream pluginv1.LoadDriver_WatchServer) error {
	runRef := req.GetRunRef()
	if runRef == "" {
		return fmt.Errorf("loaddriver-kapture: Watch requires a run_ref")
	}
	ctx := stream.Context()

	// TODO(m1): replace synthetic progress with a watch on CaptureLoadTest.status
	// per-cell rollups (sent/failed counts, achieved RPS, abort-policy state).
	progress := []*pluginv1.WatchEvent{
		{Phase: "Warmup", SentRequests: 0, FailedRequests: 0, AchievedRps: 0},
		{Phase: "Running", SentRequests: 5000, FailedRequests: 3, AchievedRps: 1000},
		{Phase: "Running", SentRequests: 15000, FailedRequests: 7, AchievedRps: 1000},
	}
	for _, ev := range progress {
		if err := stream.Send(ev); err != nil {
			return fmt.Errorf("loaddriver-kapture: send watch event: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(watchStepInterval):
		}
	}

	terminal := &pluginv1.WatchEvent{
		Phase:          "Completed",
		SentRequests:   15000,
		FailedRequests: 7,
		AchievedRps:    1000,
		Aborted:        false,
	}
	if err := stream.Send(terminal); err != nil {
		return fmt.Errorf("loaddriver-kapture: send terminal watch event: %w", err)
	}
	return nil
}

// Stop ends the run and returns an aggregated run report.
func (s *server) Stop(_ context.Context, req *pluginv1.StopRequest) (*pluginv1.StopResponse, error) {
	// TODO(m1): read the final CaptureLoadTest.status cell rollups and aggregate the
	// per-shard TrafficReplay run reports (sent/failed/filtered, latency percentiles)
	// into this RunReport. Zeros are honest placeholders until then.
	logger.Printf("stop run %s", req.GetRunRef())
	return &pluginv1.StopResponse{
		Ok:     true,
		Report: &pluginv1.RunReport{},
	}, nil
}

func main() {
	impl := &server{}
	cfg := plugin.ServeConfig{
		Name: "kapture",
		Kind: pluginv1.PluginKind_PLUGIN_KIND_LOADDRIVER,
		Lifecycle: &plugin.BaseLifecycle{
			Name: "kapture",
			Kind: pluginv1.PluginKind_PLUGIN_KIND_LOADDRIVER,
		},
		LoadDriver: impl,
	}
	if err := plugin.Serve(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

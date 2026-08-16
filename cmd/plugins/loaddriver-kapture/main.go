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
// This plugin owns the *replay* vocabulary — engine, rate mode, cell distribution,
// abort policy. None of it appears in the Study CRD any more: parallax hands the
// driver an opaque config block and this file is where it acquires meaning
// (docs/GENERALIZATION.md G1).
//
// M0 is a skeleton: Plan derives the shard count, Start synthesizes a run reference,
// Progress reports synthetic counters, and Stop returns them as summary metrics.
// TODO(m1) markers flag where the real CaptureLoadTest lifecycle will land.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/aburan28/parallax/pkg/plugin"
	pluginv1 "github.com/aburan28/parallax/proto/plugin/v1"
)

// logger writes to stderr only; stdout is reserved for the SDK handshake line.
var logger = log.New(os.Stderr, "loaddriver-kapture: ", log.LstdFlags|log.Lmsgprefix)

// workload is the block parallax passes to Start/Plan: the study's Workload, with the
// driver's own config nested under `driver.config`.
type workload struct {
	Name   string `json:"name"`
	Driver struct {
		Plugin string       `json:"plugin"`
		Config driverConfig `json:"config"`
	} `json:"driver"`
	DatasetRef *struct {
		Name string `json:"name"`
	} `json:"datasetRef"`
	TrialID string `json:"trialID"`
}

// driverConfig mirrors the CaptureLoadTest.spec fields this driver understands
// (DESIGN.md App. A.2). This is the schema the plugin publishes via Describe() and
// the study writes under workloads[].driver.config.
type driverConfig struct {
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
		TimeScale         string  `json:"timeScale"`
	} `json:"rate"`
	Abort struct {
		MaxDuration       string  `json:"maxDuration"`
		ErrorPercent      float64 `json:"errorPercent"`
		MinSampleRequests int64   `json:"minSampleRequests"`
	} `json:"abort"`
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
	d := w.Driver.Config.Distribution
	spokes := d.MaxSpokes
	if spokes <= 0 {
		spokes = 1
	}
	workers := d.WorkersPerSpoke
	if workers <= 0 {
		workers = 1
	}
	return spokes * workers
}

func (w *workload) engineOrDefault() string {
	if e := w.Driver.Config.Engine; e != "" {
		return e
	}
	return "builtin"
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
		Ok:     true,
		Detail: fmt.Sprintf("loaddriver-kapture: planned %d replay shard(s) with engine %q", shards, w.engineOrDefault()),
		Plan:   map[string]float64{"total_shards": float64(shards)},
	}, nil
}

// Start begins a load run and returns an opaque handle for Progress/Stop.
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

// Progress reports how the run is going. A replay is a continuous workload, so it
// never reports done: the study's measurement window closes it.
func (s *server) Progress(_ context.Context, req *pluginv1.ProgressRequest) (*pluginv1.ProgressResponse, error) {
	if req.GetRunRef() == "" {
		return nil, fmt.Errorf("loaddriver-kapture: Progress requires a run_ref")
	}
	// TODO(m1): read CaptureLoadTest.status per-cell rollups (sent/failed counts,
	// achieved RPS) and surface the abort-policy state as Aborted/AbortReason.
	return &pluginv1.ProgressResponse{
		Phase: "Running",
		Metrics: map[string]float64{
			"sent_requests":   0,
			"failed_requests": 0,
			"achieved_rps":    0,
		},
	}, nil
}

// Stop ends the run and returns its aggregated metrics. The metric names here are
// this driver's published contract: studies reference them from `slis[].driver`.
func (s *server) Stop(_ context.Context, req *pluginv1.StopRequest) (*pluginv1.StopResponse, error) {
	// TODO(m1): read the final CaptureLoadTest.status cell rollups and aggregate the
	// per-shard TrafficReplay run reports into these metrics. Zeros are honest
	// placeholders until then.
	logger.Printf("stop run %s", req.GetRunRef())
	return &pluginv1.StopResponse{
		Ok: true,
		Metrics: map[string]float64{
			"total_requests":    0,
			"sent_requests":     0,
			"failed_requests":   0,
			"filtered_requests": 0,
			"duration_ms":       0,
			"achieved_rps":      0,
			"mean_latency_ms":   0,
			"p50_latency_ms":    0,
			"p95_latency_ms":    0,
			"p99_latency_ms":    0,
		},
		Detail: "loaddriver-kapture: zeroed run report (M0 skeleton)",
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

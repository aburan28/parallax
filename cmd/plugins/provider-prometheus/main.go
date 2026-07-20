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

// Command provider-prometheus is a first-party parallax Provider plugin (DESIGN.md
// §5.1, §11). It evaluates SLI queries against any PromQL-compatible endpoint
// (Prometheus, Thanos, Mimir, VictoriaMetrics, Cortex) over a trial's recorded
// measurement window and returns scalar SLI values plus evidence.
//
// It is launched by the parallax host, not as a bare CLI: Serve refuses to run
// without the magic-cookie env. All diagnostics go to stderr; the SDK owns stdout.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/aburan28/parallax/pkg/plugin"
	pluginv1 "github.com/aburan28/parallax/proto/plugin/v1"
)

// logger writes to stderr only; stdout is reserved for the SDK handshake line.
var logger = log.New(os.Stderr, "provider-prometheus: ", log.LstdFlags|log.Lmsgprefix)

// configSchema is the JSON Schema (draft 2020-12) for this provider's config block.
const configSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "properties": {
    "endpoint": {
      "type": "string",
      "description": "Base URL of a PromQL-compatible query endpoint, e.g. http://prometheus.monitoring:9090"
    }
  },
  "required": ["endpoint"]
}`

// config is the CR-supplied config block for provider-prometheus.
type config struct {
	Endpoint string `json:"endpoint"`
}

// server implements the Provider service. The measured endpoint is set once by the
// host via Lifecycle.Configure and read under RLock by Collect.
type server struct {
	pluginv1.UnimplementedProviderServer

	mu       sync.RWMutex
	endpoint string
	http     *http.Client
}

// configure is wired as BaseLifecycle.ConfigureFunc: it parses {"endpoint": ...}
// and fails fast (accepted=false) on an empty or malformed config.
func (s *server) configure(configJSON []byte) (bool, string) {
	var c config
	if len(configJSON) > 0 {
		if err := json.Unmarshal(configJSON, &c); err != nil {
			return false, fmt.Sprintf("invalid provider-prometheus config: %v", err)
		}
	}
	if strings.TrimSpace(c.Endpoint) == "" {
		return false, "provider-prometheus requires a non-empty \"endpoint\""
	}
	s.mu.Lock()
	s.endpoint = strings.TrimRight(strings.TrimSpace(c.Endpoint), "/")
	s.mu.Unlock()
	logger.Printf("configured endpoint %s", c.Endpoint)
	return true, ""
}

// Capabilities advertises the trust classes this provider serves and that it does
// not support raw-series snapshots in M0.
func (s *server) Capabilities(_ context.Context, _ *pluginv1.CapabilitiesRequest) (*pluginv1.CapabilitiesResponse, error) {
	return &pluginv1.CapabilitiesResponse{
		SupportedClasses: []string{"server", "client"},
		SupportsSnapshot: false,
	}, nil
}

// Collect evaluates every SLIQuery that carries a PromQL Query over the measurement
// window and returns one SLIValue per query. Queries that carry only an expr (for
// status-scraping providers such as runreport) are skipped by this backend.
func (s *server) Collect(ctx context.Context, req *pluginv1.CollectRequest) (*pluginv1.CollectResponse, error) {
	s.mu.RLock()
	endpoint := s.endpoint
	s.mu.RUnlock()
	if endpoint == "" {
		return nil, fmt.Errorf("provider-prometheus: Collect called before Configure supplied an endpoint")
	}

	win := req.GetWindow()
	if win == nil {
		return nil, fmt.Errorf("provider-prometheus: Collect requires a measurement window")
	}
	rangeStr, evalTime, err := windowRange(win)
	if err != nil {
		return nil, fmt.Errorf("provider-prometheus: %w", err)
	}
	evalRFC3339 := evalTime.UTC().Format(time.RFC3339)

	resp := &pluginv1.CollectResponse{}
	for _, q := range req.GetQueries() {
		raw := q.GetQuery()
		if strings.TrimSpace(raw) == "" {
			// A status-scraping SLI (expr-only); not a PromQL query for this backend.
			continue
		}
		// The host leaves the range literal in place; we substitute the measured
		// duration (e.g. "300s") for the templated window.
		promQL := strings.ReplaceAll(raw, "{{.Window}}", rangeStr)

		value, ok, detail := s.query(ctx, endpoint, promQL, evalTime)
		if !ok {
			logger.Printf("query %q failed: %s", q.GetName(), detail)
		}
		resp.Values = append(resp.Values, &pluginv1.SLIValue{
			Name:               q.GetName(),
			SliClass:           q.GetSliClass(),
			Value:              value,
			Query:              promQL,
			EvaluatedAtRfc3339: evalRFC3339,
			Ok:                 ok,
			Detail:             detail,
		})
	}
	return resp, nil
}

// Snapshot would archive the raw series over the window for provenance.
func (s *server) Snapshot(_ context.Context, _ *pluginv1.SnapshotRequest) (*pluginv1.SnapshotResponse, error) {
	// TODO(m1): export the raw range series over the window to an artifact store and
	// return its URI + digest. Capabilities reports supports_snapshot=false so the
	// host does not call this in M0.
	return nil, fmt.Errorf("provider-prometheus: Snapshot is not implemented in M0")
}

func main() {
	impl := &server{
		http: &http.Client{Timeout: 30 * time.Second},
	}
	cfg := plugin.ServeConfig{
		Name: "prometheus",
		Kind: pluginv1.PluginKind_PLUGIN_KIND_PROVIDER,
		Lifecycle: &plugin.BaseLifecycle{
			Name:          "prometheus",
			Kind:          pluginv1.PluginKind_PLUGIN_KIND_PROVIDER,
			ConfigSchema:  configSchema,
			Capabilities:  []string{"server", "client"},
			ConfigureFunc: impl.configure,
		},
		Provider: impl,
	}
	if err := plugin.Serve(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

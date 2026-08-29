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

package main

import (
	"context"
	"strings"
	"testing"

	pluginv1 "github.com/aburan28/parallax/proto/plugin/v1"
)

const validSpec = `{
  "attach": {"namespace": "shop", "podSelector": {"app": "checkout"}},
  "sink": {"endpoint": "grpc://kapture-capture.kapture-system:4318", "dataset": "checkout-live"},
  "filters": [
    {"name": "no-health", "protocol": "http1", "pathPrefixes": ["/healthz"], "action": "drop"},
    {
      "name": "checkout-writes",
      "protocol": "http1",
      "ports": [8080],
      "methods": ["POST", "PUT"],
      "pathPrefixes": ["/cart", "/checkout"],
      "pathRegex": "^/api/v[0-9]+/checkout",
      "headers": [{"name": "content-type", "valuePrefix": "application/json"}],
      "statusCodes": [200, 201]
    },
    {"name": "payments", "protocol": "grpc", "methods": ["/shop.Payments/Charge"]}
  ]
}`

func TestParseSpecDefaults(t *testing.T) {
	spec, err := parseSpec([]byte(validSpec), nil)
	if err != nil {
		t.Fatalf("parseSpec: %v", err)
	}
	if spec.Attach.Mode != "socket" {
		t.Errorf("attach.mode default = %q, want socket", spec.Attach.Mode)
	}
	if spec.Sink.Compression != "zstd" {
		t.Errorf("sink.compression default = %q, want zstd", spec.Sink.Compression)
	}
	if spec.Sampling.Percent != 100 {
		t.Errorf("sampling.percent default = %v, want 100", spec.Sampling.Percent)
	}
	if got := spec.Filters[1].Action; got != "capture" {
		t.Errorf("filter action default = %q, want capture", got)
	}
}

func TestParseSpecInheritsCRDefaults(t *testing.T) {
	defaults, err := parseSpec([]byte(`{"sink": {"endpoint": "grpc://ingest:4318"}, "attach": {"mode": "uprobe-tls"}}`), nil)
	if err != nil {
		t.Fatalf("parse defaults: %v", err)
	}
	spec, err := parseSpec([]byte(`{"filters": [{"protocol": "http1"}]}`), defaults)
	if err != nil {
		t.Fatalf("parseSpec: %v", err)
	}
	if spec.Sink.Endpoint != "grpc://ingest:4318" {
		t.Errorf("sink.endpoint = %q, want inherited default", spec.Sink.Endpoint)
	}
	if spec.Attach.Mode != "uprobe-tls" {
		t.Errorf("attach.mode = %q, want inherited uprobe-tls", spec.Attach.Mode)
	}
	if spec.Filters[0].Name != "filter-0" {
		t.Errorf("filter name = %q, want auto-assigned filter-0", spec.Filters[0].Name)
	}
}

func TestValidateRejections(t *testing.T) {
	cases := []struct {
		name, spec, wantErr string
	}{
		{"missing sink", `{"filters": [{"protocol": "http1"}]}`, "sink.endpoint is required"},
		{"no filters", `{"sink": {"endpoint": "grpc://i:1"}}`, "at least one filter"},
		{"bad protocol", `{"sink": {"endpoint": "grpc://i:1"}, "filters": [{"protocol": "tcp"}]}`, "protocol"},
		{"bad port", `{"sink": {"endpoint": "grpc://i:1"}, "filters": [{"protocol": "http1", "ports": [70000]}]}`, "out of range"},
		{"bad http method", `{"sink": {"endpoint": "grpc://i:1"}, "filters": [{"protocol": "http1", "methods": ["get"]}]}`, "not an HTTP method"},
		{"bad grpc method", `{"sink": {"endpoint": "grpc://i:1"}, "filters": [{"protocol": "grpc", "methods": ["Charge"]}]}`, "full names"},
		{"bad prefix", `{"sink": {"endpoint": "grpc://i:1"}, "filters": [{"protocol": "http1", "pathPrefixes": ["cart"]}]}`, "must start with /"},
		{"bad regex", `{"sink": {"endpoint": "grpc://i:1"}, "filters": [{"protocol": "http1", "pathRegex": "["}]}`, "pathRegex"},
		{"bad status", `{"sink": {"endpoint": "grpc://i:1"}, "filters": [{"protocol": "http1", "statusCodes": [42]}]}`, "out of range"},
		{"dup names", `{"sink": {"endpoint": "grpc://i:1"}, "filters": [{"name": "a", "protocol": "http1"}, {"name": "a", "protocol": "http1"}]}`, "duplicate name"},
		{"exclusive header match", `{"sink": {"endpoint": "grpc://i:1"}, "filters": [{"protocol": "http1", "headers": [{"name": "x", "value": "a", "valuePrefix": "b"}]}]}`, "mutually exclusive"},
		{"bad sampling", `{"sink": {"endpoint": "grpc://i:1"}, "sampling": {"percent": 101}, "filters": [{"protocol": "http1"}]}`, "sampling.percent"},
		{"bad action", `{"sink": {"endpoint": "grpc://i:1"}, "filters": [{"protocol": "http1", "action": "mirror"}]}`, "action"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := parseSpec([]byte(tc.spec), nil)
			if err != nil {
				t.Fatalf("parseSpec: %v", err)
			}
			err = spec.validate(true)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("validate = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestCompileStages(t *testing.T) {
	spec, err := parseSpec([]byte(validSpec), nil)
	if err != nil {
		t.Fatalf("parseSpec: %v", err)
	}
	p, err := compile(spec)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	// no-health: 1 prefix; checkout-writes: 1 port + 2 methods + 2 prefixes +
	// 2 statuses; payments: 1 method.
	if p.KernelMapEntries != 9 {
		t.Errorf("KernelMapEntries = %d, want 9", p.KernelMapEntries)
	}
	// Only checkout-writes carries userspace-stage predicates (regex + header).
	if p.UserStageRules != 1 {
		t.Errorf("UserStageRules = %d, want 1", p.UserStageRules)
	}
	if p.pathRegexps["checkout-writes"] == nil {
		t.Errorf("compiled regex for checkout-writes missing")
	}
}

func TestSessionLifecycle(t *testing.T) {
	ctx := context.Background()
	s := &server{sessions: map[string]*session{}}

	if ok, reason := s.configure([]byte(`{"sink": {"endpoint": "grpc://ingest:4318"}}`)); !ok {
		t.Fatalf("configure rejected: %s", reason)
	}
	if ok, _ := s.configure([]byte(`{"attach": {"mode": "xdp"}}`)); ok {
		t.Fatalf("configure accepted an unknown attach mode")
	}
	// Re-set good defaults after the rejected block.
	if ok, reason := s.configure([]byte(`{"sink": {"endpoint": "grpc://ingest:4318"}}`)); !ok {
		t.Fatalf("configure rejected: %s", reason)
	}

	plan, err := s.Plan(ctx, &pluginv1.CapturePlanRequest{CaptureJson: []byte(validSpec)})
	if err != nil || !plan.GetOk() {
		t.Fatalf("Plan = %v, %v; want ok", plan.GetDetail(), err)
	}
	if plan.GetPlan()["filters"] != 3 {
		t.Errorf("plan filters = %v, want 3", plan.GetPlan()["filters"])
	}

	// A spec omitting the sink inherits the configured default and still starts.
	start, err := s.Start(ctx, &pluginv1.CaptureStartRequest{
		CaptureJson: []byte(`{"filters": [{"protocol": "http1"}]}`),
		TrialId:     "t1",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ref := start.GetSessionRef()
	if ref == "" {
		t.Fatalf("Start returned an empty session_ref")
	}

	status, err := s.Status(ctx, &pluginv1.CaptureStatusRequest{SessionRef: ref})
	if err != nil || status.GetPhase() != "Capturing" {
		t.Fatalf("Status = %v, %v; want Capturing", status.GetPhase(), err)
	}

	upd, err := s.UpdateFilters(ctx, &pluginv1.CaptureUpdateFiltersRequest{
		SessionRef:  ref,
		CaptureJson: []byte(validSpec),
	})
	if err != nil || !upd.GetOk() {
		t.Fatalf("UpdateFilters = %v, %v; want ok", upd.GetDetail(), err)
	}

	stop, err := s.Stop(ctx, &pluginv1.CaptureStopRequest{SessionRef: ref})
	if err != nil || !stop.GetOk() {
		t.Fatalf("Stop = %v, %v; want ok", stop.GetDetail(), err)
	}
	if _, ok := stop.GetMetrics()["forwarded_records"]; !ok {
		t.Errorf("Stop metrics missing forwarded_records: %v", stop.GetMetrics())
	}
	if _, err := s.Status(ctx, &pluginv1.CaptureStatusRequest{SessionRef: ref}); err == nil {
		t.Errorf("Status after Stop should fail for a removed session")
	}
}

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

// Command capture-ebpf is the first-party parallax Capture plugin (DESIGN.md
// §5.1). It compiles an L7 filter spec into a two-stage program — an in-kernel
// eBPF stage (ports, methods, path prefixes, status codes as map lookups) plus
// a userspace finishing stage (path regexes, header predicates) — attaches it
// to the selected workload's sockets, and forwards matched records to a capture
// service such as kapture's capture ingest, where they land as a versioned
// dataset replayable by loaddriver-kapture.
//
// This plugin owns the *capture* vocabulary — attach mode, L7 protocols, filter
// rules, sink. None of it appears in the core ABI: parallax hands the plugin an
// opaque capture block and filters.go is where it acquires meaning
// (docs/GENERALIZATION.md G1).
//
// M0 is a skeleton with a real front half: specs are parsed, validated, and
// compiled for real (Plan and Start share the gate), sessions are tracked, and
// UpdateFilters recompiles and swaps atomically. The kernel attach and sink
// forwarding sit behind the datapath seam; TODO(m1) markers flag where the
// cilium/ebpf loader and the capture-service client will land.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/aburan28/parallax/pkg/plugin"
	pluginv1 "github.com/aburan28/parallax/proto/plugin/v1"
)

// pluginName is the discovery/socket name (host looks for parallax-capture-ebpf).
const pluginName = "ebpf"

// logger writes to stderr only; stdout is reserved for the SDK handshake line.
var logger = log.New(os.Stderr, "capture-ebpf: ", log.LstdFlags|log.Lmsgprefix)

// configSchema is the JSON Schema (draft 2020-12) for this plugin's config
// block. The Plugin CR carries defaults (sink, attach mode); each session's
// capture_json uses the same shape and inherits what it omits.
const configSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "properties": {
    "attach": {
      "type": "object",
      "properties": {
        "namespace": {"type": "string"},
        "podSelector": {"type": "object", "additionalProperties": {"type": "string"}},
        "mode": {"enum": ["socket", "tc", "uprobe-tls"], "default": "socket"}
      }
    },
    "sink": {
      "type": "object",
      "properties": {
        "endpoint": {
          "type": "string",
          "description": "Capture service ingest matched records are forwarded to, e.g. grpc://kapture-capture.kapture-system:4318"
        },
        "dataset": {"type": "string"},
        "compression": {"enum": ["none", "gzip", "zstd"], "default": "zstd"}
      }
    },
    "sampling": {
      "type": "object",
      "properties": {
        "percent": {"type": "number", "minimum": 0, "maximum": 100, "default": 100}
      }
    },
    "filters": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["protocol"],
        "properties": {
          "name": {"type": "string"},
          "protocol": {"enum": ["http1", "http2", "grpc"]},
          "ports": {"type": "array", "items": {"type": "integer", "minimum": 1, "maximum": 65535}},
          "methods": {"type": "array", "items": {"type": "string"}},
          "pathPrefixes": {"type": "array", "items": {"type": "string", "pattern": "^/"}},
          "pathRegex": {"type": "string"},
          "headers": {
            "type": "array",
            "items": {
              "type": "object",
              "required": ["name"],
              "properties": {
                "name": {"type": "string"},
                "value": {"type": "string"},
                "valuePrefix": {"type": "string"}
              }
            }
          },
          "statusCodes": {"type": "array", "items": {"type": "integer", "minimum": 100, "maximum": 599}},
          "action": {"enum": ["capture", "drop"], "default": "capture"}
        }
      }
    }
  }
}`

// session is one live capture: a compiled program notionally attached to the
// workload, accumulating counters until Stop.
type session struct {
	ref       string
	trialID   string
	program   *compiledProgram
	startedAt time.Time

	// Counters the datapath will feed; zeros are honest placeholders in M0.
	seen, matched, forwarded, dropped, forwardedBytes float64
}

func (s *session) metrics() map[string]float64 {
	return map[string]float64{
		"seen_records":      s.seen,
		"matched_records":   s.matched,
		"forwarded_records": s.forwarded,
		"dropped_records":   s.dropped,
		"forwarded_bytes":   s.forwardedBytes,
	}
}

// server implements the Capture service. CR-supplied defaults are set once by
// the host via Lifecycle.Configure; sessions are keyed by their opaque ref.
type server struct {
	pluginv1.UnimplementedCaptureServer

	mu       sync.Mutex
	defaults *captureSpec
	sessions map[string]*session
	seq      int
}

// configure is wired as BaseLifecycle.ConfigureFunc: the CR config block is a
// partial spec (defaults), validated for coherence but not for completeness.
func (s *server) configure(configJSON []byte) (bool, string) {
	spec, err := parseSpec(configJSON, nil)
	if err != nil {
		return false, fmt.Sprintf("invalid capture-ebpf config: %v", err)
	}
	if err := spec.validate(false); err != nil {
		return false, fmt.Sprintf("invalid capture-ebpf config: %v", err)
	}
	s.mu.Lock()
	s.defaults = spec
	s.mu.Unlock()
	logger.Printf("configured defaults (sink %q, attach mode %q)", spec.Sink.Endpoint, spec.Attach.Mode)
	return true, ""
}

func (s *server) compileWithDefaults(raw []byte) (*compiledProgram, error) {
	s.mu.Lock()
	defaults := s.defaults
	s.mu.Unlock()
	spec, err := parseSpec(raw, defaults)
	if err != nil {
		return nil, err
	}
	return compile(spec)
}

// Plan validates and compiles the filter spec without attaching anything.
func (s *server) Plan(_ context.Context, req *pluginv1.CapturePlanRequest) (*pluginv1.CapturePlanResponse, error) {
	p, err := s.compileWithDefaults(req.GetCaptureJson())
	if err != nil {
		return &pluginv1.CapturePlanResponse{Ok: false, Detail: fmt.Sprintf("invalid capture spec: %v", err)}, nil
	}
	return &pluginv1.CapturePlanResponse{
		Ok: true,
		Detail: fmt.Sprintf("capture-ebpf: compiled %d filter(s) — %d kernel map entries, %d userspace-stage rule(s), sink %s",
			len(p.spec.Filters), p.KernelMapEntries, p.UserStageRules, p.spec.Sink.Endpoint),
		Plan: p.planFacts(),
	}, nil
}

// Start compiles the spec, attaches the datapath, and begins forwarding.
func (s *server) Start(_ context.Context, req *pluginv1.CaptureStartRequest) (*pluginv1.CaptureStartResponse, error) {
	p, err := s.compileWithDefaults(req.GetCaptureJson())
	if err != nil {
		return nil, fmt.Errorf("capture-ebpf: %w", err)
	}
	// TODO(m1): load the eBPF programs for attach.mode via cilium/ebpf, populate
	// the kernel-stage maps from the compiled program, attach to the selected
	// pods' sockets, and open the capture-service client toward sink.endpoint.
	s.mu.Lock()
	s.seq++
	sess := &session{
		ref:       fmt.Sprintf("cap-%s-%d", orDefault(req.GetTrialId(), "session"), s.seq),
		trialID:   req.GetTrialId(),
		program:   p,
		startedAt: time.Now(),
	}
	s.sessions[sess.ref] = sess
	s.mu.Unlock()
	logger.Printf("start session %s (%d filters)", sess.ref, len(p.spec.Filters))
	return &pluginv1.CaptureStartResponse{
		SessionRef: sess.ref,
		Detail:     "capture-ebpf: session registered (no eBPF programs attached in M0)",
	}, nil
}

// Status reports a running session's counters. A capture is a continuous
// workload: it never reports done, the trial's measurement window closes it.
func (s *server) Status(_ context.Context, req *pluginv1.CaptureStatusRequest) (*pluginv1.CaptureStatusResponse, error) {
	s.mu.Lock()
	sess, ok := s.sessions[req.GetSessionRef()]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("capture-ebpf: unknown session_ref %q", req.GetSessionRef())
	}
	// TODO(m1): read the per-CPU counter maps and the sink client's error state;
	// surface a dead sink or a kernel-detached link as Aborted/AbortReason.
	return &pluginv1.CaptureStatusResponse{
		Phase:   "Capturing",
		Metrics: sess.metrics(),
	}, nil
}

// UpdateFilters recompiles the spec and swaps the session's program in place —
// the eBPF analogue is a map rewrite, no detach, so capture never gaps.
func (s *server) UpdateFilters(_ context.Context, req *pluginv1.CaptureUpdateFiltersRequest) (*pluginv1.CaptureUpdateFiltersResponse, error) {
	p, err := s.compileWithDefaults(req.GetCaptureJson())
	if err != nil {
		return &pluginv1.CaptureUpdateFiltersResponse{Ok: false, Detail: fmt.Sprintf("invalid capture spec: %v", err)}, nil
	}
	s.mu.Lock()
	sess, ok := s.sessions[req.GetSessionRef()]
	if ok {
		sess.program = p
	}
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("capture-ebpf: unknown session_ref %q", req.GetSessionRef())
	}
	// TODO(m1): rewrite the kernel-stage maps from the new compiled program.
	logger.Printf("session %s: filters swapped (%d filters)", sess.ref, len(p.spec.Filters))
	return &pluginv1.CaptureUpdateFiltersResponse{
		Ok:     true,
		Detail: fmt.Sprintf("capture-ebpf: %d filter(s) live", len(p.spec.Filters)),
	}, nil
}

// Stop detaches, flushes, and returns the session's summary. The metric names
// here are this plugin's published contract (see CaptureStopResponse).
func (s *server) Stop(_ context.Context, req *pluginv1.CaptureStopRequest) (*pluginv1.CaptureStopResponse, error) {
	s.mu.Lock()
	sess, ok := s.sessions[req.GetSessionRef()]
	delete(s.sessions, req.GetSessionRef())
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("capture-ebpf: unknown session_ref %q", req.GetSessionRef())
	}
	// TODO(m1): detach links, drain the ring buffer, flush the sink client, and
	// fold the final counter-map readings into these metrics.
	logger.Printf("stop session %s", sess.ref)
	metrics := sess.metrics()
	metrics["duration_ms"] = float64(time.Since(sess.startedAt).Milliseconds())
	return &pluginv1.CaptureStopResponse{
		Ok:      true,
		Metrics: metrics,
		Detail:  "capture-ebpf: zeroed session report (M0 skeleton)",
	}, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func main() {
	impl := &server{sessions: map[string]*session{}}
	cfg := plugin.ServeConfig{
		Name: pluginName,
		Kind: pluginv1.PluginKind_PLUGIN_KIND_CAPTURE,
		Lifecycle: &plugin.BaseLifecycle{
			Name:          pluginName,
			Kind:          pluginv1.PluginKind_PLUGIN_KIND_CAPTURE,
			ConfigSchema:  configSchema,
			Capabilities:  []string{"l7-filters", "http1", "http2", "grpc", "live-filter-swap"},
			ConfigureFunc: impl.configure,
		},
		Capture: impl,
	}
	if err := plugin.Serve(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

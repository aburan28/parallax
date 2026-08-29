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
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// This file owns the eBPF L7 filter vocabulary. The Capture ABI hands the plugin
// an opaque capture block (docs/GENERALIZATION.md G1); these types are where it
// acquires meaning. The same spec shape is used for the Plugin CR config block
// (defaults) and the per-session capture_json (which inherits missing defaults).

// captureSpec is the resolved capture block: where to attach, what to match, and
// where matched records go.
type captureSpec struct {
	Attach   attachSpec `json:"attach"`
	Sink     sinkSpec   `json:"sink"`
	Sampling struct {
		// Percent of matched records forwarded, 0–100. 0 means "unset" and
		// defaults to 100 (capture everything that matches).
		Percent float64 `json:"percent"`
	} `json:"sampling"`
	Filters []l7Filter `json:"filters"`
}

// attachSpec selects the workload and the datapath hook.
type attachSpec struct {
	// Namespace + podSelector choose the pods whose sockets the datapath taps.
	Namespace   string            `json:"namespace"`
	PodSelector map[string]string `json:"podSelector"`
	// Mode is the eBPF hook family: "socket" (sockops/sk_msg, plaintext),
	// "tc" (tc clsact ingress/egress), or "uprobe-tls" (uprobes on the TLS
	// library's read/write, seeing plaintext before encryption).
	Mode string `json:"mode"`
}

// sinkSpec names the capture service matched records are forwarded to — e.g.
// kapture's capture ingest, where they land as a versioned dataset.
type sinkSpec struct {
	// Endpoint of the capture service ingest, e.g.
	// grpc://kapture-capture.kapture-system:4318.
	Endpoint string `json:"endpoint"`
	// Dataset the forwarded records are appended to.
	Dataset string `json:"dataset"`
	// Compression for forwarded batches: "none", "gzip", or "zstd" (default).
	Compression string `json:"compression"`
}

// l7Filter is one match rule. Rules are evaluated first-match-wins in spec
// order; a record matching no rule is not captured (allowlist semantics).
type l7Filter struct {
	Name     string `json:"name"`
	Protocol string `json:"protocol"` // http1 | http2 | grpc
	// Ports narrow the tap to these L4 ports (empty = all tapped ports).
	Ports []int32 `json:"ports"`
	// Methods are HTTP methods (GET, POST, …) for http1/http2, or full gRPC
	// method names (/pkg.Service/Method) for grpc.
	Methods []string `json:"methods"`
	// PathPrefixes match the request path (the :path pseudo-header for h2/grpc).
	PathPrefixes []string `json:"pathPrefixes"`
	// PathRegex is a userspace-stage predicate: regexes cannot run in-kernel, so
	// a rule carrying one is kernel-prefiltered and finished in userspace.
	PathRegex string `json:"pathRegex"`
	// Headers are userspace-stage predicates on decoded request headers.
	Headers []headerMatch `json:"headers"`
	// StatusCodes match the response side; a rule with status predicates
	// captures the exchange only once the response is seen.
	StatusCodes []int32 `json:"statusCodes"`
	// Action for a matching record: "capture" (default) or "drop". A drop rule
	// ahead of a capture rule excludes a subset (e.g. drop /healthz, capture /).
	Action string `json:"action"`
}

type headerMatch struct {
	Name string `json:"name"`
	// Exactly one of value / valuePrefix; empty both ⇒ presence match.
	Value       string `json:"value"`
	ValuePrefix string `json:"valuePrefix"`
}

var (
	knownProtocols   = map[string]bool{"http1": true, "http2": true, "grpc": true}
	knownAttachModes = map[string]bool{"socket": true, "tc": true, "uprobe-tls": true}
	knownCompression = map[string]bool{"none": true, "gzip": true, "zstd": true}
	httpMethods      = map[string]bool{
		"GET": true, "HEAD": true, "POST": true, "PUT": true, "PATCH": true,
		"DELETE": true, "OPTIONS": true, "TRACE": true, "CONNECT": true,
	}
)

// parseSpec decodes a capture block and fills defaults from the Plugin CR
// config (which may be nil). Per-session fields win over CR defaults.
func parseSpec(raw []byte, defaults *captureSpec) (*captureSpec, error) {
	var s captureSpec
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("decode capture spec: %w", err)
		}
	}
	if defaults != nil {
		if s.Sink.Endpoint == "" {
			s.Sink.Endpoint = defaults.Sink.Endpoint
		}
		if s.Sink.Dataset == "" {
			s.Sink.Dataset = defaults.Sink.Dataset
		}
		if s.Sink.Compression == "" {
			s.Sink.Compression = defaults.Sink.Compression
		}
		if s.Attach.Mode == "" {
			s.Attach.Mode = defaults.Attach.Mode
		}
	}
	if s.Attach.Mode == "" {
		s.Attach.Mode = "socket"
	}
	if s.Sink.Compression == "" {
		s.Sink.Compression = "zstd"
	}
	if s.Sampling.Percent == 0 {
		s.Sampling.Percent = 100
	}
	for i := range s.Filters {
		f := &s.Filters[i]
		if f.Name == "" {
			f.Name = fmt.Sprintf("filter-%d", i)
		}
		if f.Action == "" {
			f.Action = "capture"
		}
	}
	return &s, nil
}

// validate fail-fasts a full session spec. requireSession demands the fields a
// running session needs (sink, filters); the CR defaults block is validated
// with requireSession=false since it only has to be a coherent partial spec.
func (s *captureSpec) validate(requireSession bool) error {
	if !knownAttachModes[s.Attach.Mode] {
		return fmt.Errorf("attach.mode %q: must be one of socket, tc, uprobe-tls", s.Attach.Mode)
	}
	if !knownCompression[s.Sink.Compression] {
		return fmt.Errorf("sink.compression %q: must be one of none, gzip, zstd", s.Sink.Compression)
	}
	if s.Sampling.Percent < 0 || s.Sampling.Percent > 100 {
		return fmt.Errorf("sampling.percent %v: must be within 0–100", s.Sampling.Percent)
	}
	if requireSession {
		if strings.TrimSpace(s.Sink.Endpoint) == "" {
			return fmt.Errorf("sink.endpoint is required: the capture service matched records are forwarded to")
		}
		if len(s.Filters) == 0 {
			return fmt.Errorf("at least one filter is required (capture is allowlist-only)")
		}
	}
	seen := map[string]bool{}
	for i := range s.Filters {
		f := &s.Filters[i]
		if seen[f.Name] {
			return fmt.Errorf("filter %q: duplicate name", f.Name)
		}
		seen[f.Name] = true
		if err := f.validate(); err != nil {
			return fmt.Errorf("filter %q: %w", f.Name, err)
		}
	}
	return nil
}

func (f *l7Filter) validate() error {
	if !knownProtocols[f.Protocol] {
		return fmt.Errorf("protocol %q: must be one of http1, http2, grpc", f.Protocol)
	}
	if f.Action != "capture" && f.Action != "drop" {
		return fmt.Errorf("action %q: must be capture or drop", f.Action)
	}
	for _, p := range f.Ports {
		if p < 1 || p > 65535 {
			return fmt.Errorf("port %d: out of range 1–65535", p)
		}
	}
	for _, m := range f.Methods {
		if f.Protocol == "grpc" {
			if !strings.HasPrefix(m, "/") || strings.Count(m, "/") != 2 {
				return fmt.Errorf("method %q: grpc methods are full names like /pkg.Service/Method", m)
			}
		} else if !httpMethods[m] {
			return fmt.Errorf("method %q: not an HTTP method (methods are uppercase, e.g. GET)", m)
		}
	}
	for _, p := range f.PathPrefixes {
		if !strings.HasPrefix(p, "/") {
			return fmt.Errorf("pathPrefix %q: must start with /", p)
		}
	}
	if f.PathRegex != "" {
		if _, err := regexp.Compile(f.PathRegex); err != nil {
			return fmt.Errorf("pathRegex: %v", err)
		}
	}
	for _, h := range f.Headers {
		if strings.TrimSpace(h.Name) == "" {
			return fmt.Errorf("header match: name is required")
		}
		if h.Value != "" && h.ValuePrefix != "" {
			return fmt.Errorf("header %q: value and valuePrefix are mutually exclusive", h.Name)
		}
	}
	for _, c := range f.StatusCodes {
		if c < 100 || c > 599 {
			return fmt.Errorf("statusCode %d: out of range 100–599", c)
		}
	}
	return nil
}

// compiledProgram is the two-stage form the datapath loads. The kernel stage is
// everything expressible as eBPF map lookups and bounded prefix compares
// (protocol, ports, methods, path prefixes, status codes); the userspace stage
// finishes rules whose predicates cannot run in-kernel (path regexes, header
// matches) after the kernel stage has prefiltered.
type compiledProgram struct {
	spec *captureSpec

	// KernelMapEntries counts the map slots the kernel stage loads: one per
	// port, method, path prefix, and status code across all rules.
	KernelMapEntries int
	// UserStageRules counts rules that need the userspace finishing stage.
	UserStageRules int
	// pathRegexps holds the compiled userspace-stage regexes, keyed by rule name.
	pathRegexps map[string]*regexp.Regexp
}

// compile validates and lowers a session spec. It is the single gate both Plan
// and Start go through, so a spec that plans clean also starts clean.
func compile(spec *captureSpec) (*compiledProgram, error) {
	if err := spec.validate(true); err != nil {
		return nil, err
	}
	p := &compiledProgram{spec: spec, pathRegexps: map[string]*regexp.Regexp{}}
	for i := range spec.Filters {
		f := &spec.Filters[i]
		p.KernelMapEntries += len(f.Ports) + len(f.Methods) + len(f.PathPrefixes) + len(f.StatusCodes)
		if f.PathRegex != "" || len(f.Headers) > 0 {
			p.UserStageRules++
			if f.PathRegex != "" {
				p.pathRegexps[f.Name] = regexp.MustCompile(f.PathRegex) // validated above
			}
		}
	}
	return p, nil
}

// planFacts summarizes a compiled program for CapturePlanResponse.plan.
func (p *compiledProgram) planFacts() map[string]float64 {
	return map[string]float64{
		"filters":            float64(len(p.spec.Filters)),
		"kernel_map_entries": float64(p.KernelMapEntries),
		"user_stage_rules":   float64(p.UserStageRules),
		"sampling_percent":   p.spec.Sampling.Percent,
	}
}

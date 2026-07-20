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

// Command target-kapture is a first-party parallax Target plugin (DESIGN.md §5.1,
// §8.1). It owns the kapture system-under-test: it applies a config point, gates on
// health, resets between trials, and validates dimension names at admission via
// Contract. It supports both direct and integrated modes (§8.1).
//
// M0 is a skeleton: Prepare/Apply/Ready/Reset return honest, well-formed responses
// with TODO(m1) markers where real chart/CR rollout will land. Contract is real.
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	"github.com/aburan28/parallax/pkg/plugin"
	pluginv1 "github.com/aburan28/parallax/proto/plugin/v1"
)

// logger writes to stderr only; stdout is reserved for the SDK handshake line.
var logger = log.New(os.Stderr, "target-kapture: ", log.LstdFlags|log.Lmsgprefix)

// server implements the Target service for kapture.
type server struct {
	pluginv1.UnimplementedTargetServer
}

// Prepare ensures study/cluster-level prerequisites (charts, namespaces, storage).
func (s *server) Prepare(_ context.Context, _ *pluginv1.PrepareRequest) (*pluginv1.PrepareResponse, error) {
	// TODO(m1): install/verify the capture-agent chart (integrated) or provision the
	// direct-mode prerequisites (namespace, MinIO-backed storage, images).
	return &pluginv1.PrepareResponse{
		Ready:  true,
		Detail: "target-kapture: prerequisites assumed present (M0 skeleton)",
	}, nil
}

// Apply materializes one config point onto the capture-agent.
func (s *server) Apply(_ context.Context, req *pluginv1.ApplyRequest) (*pluginv1.ApplyResponse, error) {
	// TODO(m1): translate dimensions into a capture-agent Deployment patch (direct
	// mode owns the pod spec) or Helm values / CR fields (integrated mode), roll it
	// out, and return the rolled-out object refs for provenance/cleanup.
	logger.Printf("apply config %s with %d dimension(s)", req.GetConfigHash(), len(req.GetDimensions()))
	return &pluginv1.ApplyResponse{
		Applied: true,
		Detail: fmt.Sprintf("target-kapture: recorded %d dimension(s) for config %s (no rollout in M0)",
			len(req.GetDimensions()), req.GetConfigHash()),
	}, nil
}

// Ready gates on rollout + capture-agent health before load starts.
func (s *server) Ready(_ context.Context, req *pluginv1.ReadyRequest) (*pluginv1.ReadyResponse, error) {
	// TODO(m1): wait on Deployment rollout status and the capture-agent /healthz
	// (:8081) endpoint, bounded by req.timeout_seconds.
	return &pluginv1.ReadyResponse{
		Ready:  true,
		Detail: fmt.Sprintf("target-kapture: health gate assumed passed for config %s (M0 skeleton)", req.GetConfigHash()),
	}, nil
}

// Reset tears down trial state and quiesces before the next trial.
func (s *server) Reset(_ context.Context, req *pluginv1.ResetRequest) (*pluginv1.ResetResponse, error) {
	// TODO(m1): delete the trial's TrafficCapture/CaptureLoadTest, clear the storage
	// prefix, restart capture-agent pods when cold_start is set, and verify quiesce
	// (rate(capture_agent_requests_total) ~= 0, queue depth 0) before returning.
	return &pluginv1.ResetResponse{
		Ok: true,
		Detail: fmt.Sprintf("target-kapture: reset acknowledged (coldStart=%t) for config %s (M0 skeleton)",
			req.GetColdStart(), req.GetConfigHash()),
	}, nil
}

// Contract validates the requested dimension names against the known kapture
// dimension paths and reports the modes this target supports. It is real in M0:
// admission uses it to reject unknown dimensions before any trial is spent.
func (s *server) Contract(_ context.Context, req *pluginv1.ContractRequest) (*pluginv1.ContractResponse, error) {
	unknown := unknownDimensions(req.GetDimensionNames())
	return &pluginv1.ContractResponse{
		Ok:                len(unknown) == 0,
		UnknownDimensions: unknown,
		SupportedModes:    supportedModes(),
	}, nil
}

func main() {
	impl := &server{}
	cfg := plugin.ServeConfig{
		Name: "kapture",
		Kind: pluginv1.PluginKind_PLUGIN_KIND_TARGET,
		Lifecycle: &plugin.BaseLifecycle{
			Name:         "kapture",
			Kind:         pluginv1.PluginKind_PLUGIN_KIND_TARGET,
			Capabilities: supportedModes(),
		},
		Target: impl,
	}
	if err := plugin.Serve(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

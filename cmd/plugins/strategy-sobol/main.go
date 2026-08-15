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

// Command strategy-sobol is the first-party "sobol" strategy plugin. It drives
// the search using a Sobol low-discrepancy quasi-random sequence.
//
// M0 skeleton: the Strategy RPCs are intentionally left unimplemented via the
// embedded UnimplementedStrategyServer so the binary builds, handshakes, and
// serves.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/aburan28/parallax/pkg/plugin"
	pluginv1 "github.com/aburan28/parallax/proto/plugin/v1"
)

// pluginName is the discovery/socket name (host looks for parallax-strategy-sobol).
const pluginName = "sobol"

// server implements the Strategy service. Its RPCs remain unimplemented for M0
// via the embedded UnimplementedStrategyServer.
//
// TODO(m3): implement Strategy RPCs (Sobol sequence generation, Ask/Tell/Report).
type server struct {
	pluginv1.UnimplementedStrategyServer
}

// Ask fails loudly rather than inheriting the generic "method not implemented"
// from the embedded UnimplementedStrategyServer. A study naming this strategy fails
// with a reason that says what is missing, instead of quietly searching something
// else — the failure mode this seam was rebuilt to remove.
func (s *server) Ask(_ context.Context, _ *pluginv1.AskRequest) (*pluginv1.AskResponse, error) {
	return nil, fmt.Errorf("strategy-sobol is not implemented yet (a Sobol low-discrepancy sequence); " +
		"use builtin:grid, builtin:random or strategy-random until it lands — see docs/DESIGN.md §14")
}

func main() {
	impl := &server{}
	cfg := plugin.ServeConfig{
		Name: pluginName,
		Kind: pluginv1.PluginKind_PLUGIN_KIND_STRATEGY,
		Lifecycle: &plugin.BaseLifecycle{
			Name: pluginName,
			Kind: pluginv1.PluginKind_PLUGIN_KIND_STRATEGY,
		},
		Strategy: impl,
	}
	if err := plugin.Serve(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

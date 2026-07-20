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

// Command scenario-netpol-outage is the first-party "netpol-outage" scenario
// plugin. It injects a network-partition fault by applying a deny NetworkPolicy
// to the system-under-test during a trial.
//
// M0 skeleton: the Scenario RPCs are intentionally left unimplemented via the
// embedded UnimplementedScenarioServer so the binary builds, handshakes, and
// serves.
package main

import (
	"fmt"
	"os"

	"github.com/aburan28/parallax/pkg/plugin"
	pluginv1 "github.com/aburan28/parallax/proto/plugin/v1"
)

// pluginName is the discovery/socket name (host looks for parallax-scenario-netpol-outage).
const pluginName = "netpol-outage"

// server implements the Scenario service. Its RPCs remain unimplemented for M0
// via the embedded UnimplementedScenarioServer.
//
// TODO(m2): implement Scenario RPCs (apply deny NetworkPolicy, observe, restore).
type server struct {
	pluginv1.UnimplementedScenarioServer
}

func main() {
	impl := &server{}
	cfg := plugin.ServeConfig{
		Name: pluginName,
		Kind: pluginv1.PluginKind_PLUGIN_KIND_SCENARIO,
		Lifecycle: &plugin.BaseLifecycle{
			Name: pluginName,
			Kind: pluginv1.PluginKind_PLUGIN_KIND_SCENARIO,
		},
		Scenario: impl,
	}
	if err := plugin.Serve(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

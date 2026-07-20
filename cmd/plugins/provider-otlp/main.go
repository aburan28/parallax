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

// Command provider-otlp is the first-party "otlp" provider plugin. It sources
// SLI values from an OpenTelemetry (OTLP) metrics endpoint.
//
// M0 skeleton: the Provider RPCs are intentionally left unimplemented via the
// embedded UnimplementedProviderServer so the binary builds, handshakes, and
// serves.
package main

import (
	"fmt"
	"os"

	"github.com/aburan28/parallax/pkg/plugin"
	pluginv1 "github.com/aburan28/parallax/proto/plugin/v1"
)

// pluginName is the discovery/socket name (host looks for parallax-provider-otlp).
const pluginName = "otlp"

// server implements the Provider service. Its RPCs remain unimplemented for M0
// via the embedded UnimplementedProviderServer.
//
// TODO(m2): implement Provider RPCs (query OTLP metrics, resolve SLIs).
type server struct {
	pluginv1.UnimplementedProviderServer
}

func main() {
	impl := &server{}
	cfg := plugin.ServeConfig{
		Name: pluginName,
		Kind: pluginv1.PluginKind_PLUGIN_KIND_PROVIDER,
		Lifecycle: &plugin.BaseLifecycle{
			Name: pluginName,
			Kind: pluginv1.PluginKind_PLUGIN_KIND_PROVIDER,
		},
		Provider: impl,
	}
	if err := plugin.Serve(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

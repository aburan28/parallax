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

// Command exporter-ocibundle is the first-party "ocibundle" exporter plugin. It
// packages study artifacts into an OCI image bundle and pushes it to a registry.
//
// M0 skeleton: the Exporter RPC is intentionally left unimplemented via the
// embedded UnimplementedExporterServer so the binary builds, handshakes, and
// serves.
package main

import (
	"fmt"
	"os"

	"github.com/aburan28/parallax/pkg/plugin"
	pluginv1 "github.com/aburan28/parallax/proto/plugin/v1"
)

// pluginName is the discovery/socket name (host looks for parallax-exporter-ocibundle).
const pluginName = "ocibundle"

// server implements the Exporter service. Its RPC remains unimplemented for M0
// via the embedded UnimplementedExporterServer.
//
// TODO(m3): implement Export (assemble OCI artifact bundle, push to registry).
type server struct {
	pluginv1.UnimplementedExporterServer
}

func main() {
	impl := &server{}
	cfg := plugin.ServeConfig{
		Name: pluginName,
		Kind: pluginv1.PluginKind_PLUGIN_KIND_EXPORTER,
		Lifecycle: &plugin.BaseLifecycle{
			Name: pluginName,
			Kind: pluginv1.PluginKind_PLUGIN_KIND_EXPORTER,
		},
		Exporter: impl,
	}
	if err := plugin.Serve(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

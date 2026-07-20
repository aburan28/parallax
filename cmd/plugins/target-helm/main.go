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

// Command target-helm is the first-party "helm" target plugin. It renders and
// applies a Helm-defined system-under-test as the deployment target of a study.
//
// M0 skeleton: the Target RPCs are intentionally left unimplemented via the
// embedded UnimplementedTargetServer so the binary builds, handshakes, and
// serves. Real Helm rendering/apply lands later.
package main

import (
	"fmt"
	"os"

	"github.com/aburan28/parallax/pkg/plugin"
	pluginv1 "github.com/aburan28/parallax/proto/plugin/v1"
)

// pluginName is the discovery/socket name (host looks for parallax-target-helm).
const pluginName = "helm"

// server implements the Target service. Its RPCs remain unimplemented for M0
// via the embedded UnimplementedTargetServer.
//
// TODO(m1): implement Target RPCs (render Helm chart, apply, readiness, teardown).
type server struct {
	pluginv1.UnimplementedTargetServer
}

func main() {
	impl := &server{}
	cfg := plugin.ServeConfig{
		Name: pluginName,
		Kind: pluginv1.PluginKind_PLUGIN_KIND_TARGET,
		Lifecycle: &plugin.BaseLifecycle{
			Name: pluginName,
			Kind: pluginv1.PluginKind_PLUGIN_KIND_TARGET,
		},
		Target: impl,
	}
	if err := plugin.Serve(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

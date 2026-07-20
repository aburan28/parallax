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

// Package pluginhost is the subprocess plugin supervisor (DESIGN.md §5.2, §5.4): it
// installs plugin binaries, launches them, performs the handshake, runs the health
// supervisor loop, hot-reloads on binary change with drain-and-swap, and hands typed
// gRPC clients to controllers. The concrete implementation lives in host_impl.go.
package pluginhost

import (
	"context"

	"google.golang.org/grpc"

	v1alpha1 "github.com/aburan28/parallax/api/v1alpha1"
)

// Host manages the lifecycle of plugin subprocesses and vends connections. Callers
// build typed clients from a connection, e.g.:
//
//	conn, _ := host.Conn("target-kapture")
//	tc := pluginv1.NewTargetClient(conn)
type Host interface {
	// Ensure installs, verifies, loads (or hot-reloads) the plugin described by a
	// Plugin CR, driving it toward Ready. It is idempotent and safe to call on every
	// reconcile. On success the CR's status fields (digest, ABI, phase) are updated
	// on the passed-in copy.
	Ensure(ctx context.Context, p *v1alpha1.Plugin) error

	// Unload drains (bounded grace) and stops a plugin by name.
	Unload(ctx context.Context, name string) error

	// Conn returns a connected gRPC client conn for a Ready plugin, or an error if the
	// plugin is unknown or not Ready.
	Conn(name string) (*grpc.ClientConn, error)

	// Ready reports whether a plugin is loaded and healthy.
	Ready(name string) bool

	// Digest returns the resolved image digest of a loaded plugin, for the trial
	// environment fingerprint (DESIGN.md §9). ok is false if the plugin is unknown.
	Digest(name string) (digest string, ok bool)

	// Close drains and stops every plugin.
	Close(ctx context.Context) error
}

// Options configure a Host.
type Options struct {
	// PluginDir is the shared volume plugin binaries are installed into (default /plugins).
	PluginDir string
	// SocketDir is where plugin unix sockets are created (default os.TempDir()).
	SocketDir string
	// HostABIVersions is offered during Describe negotiation (default {"v1"}).
	HostABIVersions []string
	// HealthInterval is the supervisor poll period.
	HealthIntervalSeconds int
	// SkipVerify disables cosign verification (dev/--local only). §5.3.
	SkipVerify bool
}

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

// Package plugin is the Go SDK for parallax gRPC subprocess plugins (DESIGN.md §5.2).
// A plugin main constructs a ServeConfig and calls Serve; the host uses ParseHandshake
// and Dial to connect. The conventions deliberately mirror kapture's replay-engine ABI
// (App. A.2), generalized across the seven plugin kinds.
package plugin

import (
	"fmt"
	"strings"
	"time"

	pluginv1 "github.com/aburan28/parallax/proto/plugin/v1"
)

const (
	// MagicEnv/MagicValue form the magic cookie the host sets so a plugin binary
	// refuses to run as a bare CLI (kapture convention).
	MagicEnv   = "PARALLAX_PLUGIN_MAGIC"
	MagicValue = "parallax-plugin"

	// SocketDirEnv names the directory the plugin should create its unix socket in.
	SocketDirEnv = "PARALLAX_PLUGIN_SOCKET_DIR"

	// HandshakePrefix begins the single stdout handshake line:
	//   PARALLAX-PLUGIN|1|unix|<socket-path>|grpc
	HandshakePrefix = "PARALLAX-PLUGIN"

	// HandshakeProtocolVersion is the wire version of the handshake line itself.
	HandshakeProtocolVersion = "1"

	// ABIVersion is the plugin ABI version this SDK implements (DESIGN.md §5.2).
	ABIVersion = "v1"

	// HandshakeTimeout bounds how long the host waits for the handshake line.
	HandshakeTimeout = 15 * time.Second

	// DefaultDrainGrace is the default bounded grace for Drain (DESIGN.md §5.4).
	DefaultDrainGrace = 30 * time.Second
)

// Handshake is the parsed stdout handshake line emitted by a plugin.
type Handshake struct {
	ProtocolVersion string
	Network         string // "unix"
	Address         string // socket path
	Transport       string // "grpc"
}

// FormatHandshake renders the single handshake line a plugin prints to stdout.
func FormatHandshake(socketPath string) string {
	return strings.Join([]string{
		HandshakePrefix, HandshakeProtocolVersion, "unix", socketPath, "grpc",
	}, "|")
}

// ParseHandshake parses a plugin's handshake line. Used by the host.
func ParseHandshake(line string) (Handshake, error) {
	line = strings.TrimSpace(line)
	parts := strings.Split(line, "|")
	if len(parts) != 5 || parts[0] != HandshakePrefix {
		return Handshake{}, fmt.Errorf("malformed handshake line %q", line)
	}
	h := Handshake{
		ProtocolVersion: parts[1],
		Network:         parts[2],
		Address:         parts[3],
		Transport:       parts[4],
	}
	if h.Network != "unix" || h.Transport != "grpc" {
		return Handshake{}, fmt.Errorf("unsupported handshake transport: %s/%s", h.Network, h.Transport)
	}
	return h, nil
}

// ServeConfig declares what a plugin binary serves. Lifecycle is required; exactly
// one kind service must be set matching Kind.
type ServeConfig struct {
	Name      string
	Kind      pluginv1.PluginKind
	Lifecycle pluginv1.LifecycleServer

	// Set exactly the one matching Kind; leave the rest nil.
	Target     pluginv1.TargetServer
	LoadDriver pluginv1.LoadDriverServer
	Provider   pluginv1.ProviderServer
	Strategy   pluginv1.StrategyServer
	Scenario   pluginv1.ScenarioServer
	Exporter   pluginv1.ExporterServer
	Capture    pluginv1.CaptureServer
}

// KindString returns the short kind string ("target", "provider", …) for discovery.
func KindString(k pluginv1.PluginKind) string {
	switch k {
	case pluginv1.PluginKind_PLUGIN_KIND_TARGET:
		return "target"
	case pluginv1.PluginKind_PLUGIN_KIND_LOADDRIVER:
		return "loaddriver"
	case pluginv1.PluginKind_PLUGIN_KIND_PROVIDER:
		return "provider"
	case pluginv1.PluginKind_PLUGIN_KIND_STRATEGY:
		return "strategy"
	case pluginv1.PluginKind_PLUGIN_KIND_SCENARIO:
		return "scenario"
	case pluginv1.PluginKind_PLUGIN_KIND_EXPORTER:
		return "exporter"
	case pluginv1.PluginKind_PLUGIN_KIND_CAPTURE:
		return "capture"
	default:
		return "unknown"
	}
}

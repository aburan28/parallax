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

package plugin

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"google.golang.org/grpc"

	pluginv1 "github.com/aburan28/parallax/proto/plugin/v1"
)

// Serve runs a plugin: it validates the magic cookie, creates a unix socket in the
// host-provided socket dir, registers the Lifecycle service plus the one kind service,
// prints exactly one handshake line to stdout, and blocks serving until SIGTERM/SIGINT.
// All diagnostic logging must go to stderr — stdout carries only the handshake line.
func Serve(cfg ServeConfig) error {
	if os.Getenv(MagicEnv) != MagicValue {
		return fmt.Errorf("%s is a parallax plugin and must be launched by the parallax host (missing %s)", cfg.Name, MagicEnv)
	}
	if cfg.Lifecycle == nil {
		return fmt.Errorf("plugin %s: Lifecycle service is required", cfg.Name)
	}

	srv := grpc.NewServer()
	pluginv1.RegisterLifecycleServer(srv, cfg.Lifecycle)
	if err := registerKind(srv, cfg); err != nil {
		return err
	}

	dir := os.Getenv(SocketDirEnv)
	if dir == "" {
		dir = os.TempDir()
	}
	socketPath := filepath.Join(dir, fmt.Sprintf("parallax-%s-%s-%d.sock", KindString(cfg.Kind), cfg.Name, os.Getpid()))
	_ = os.Remove(socketPath) // stale socket from a crashed predecessor
	lis, err := net.Listen("unix", socketPath)
	if err != nil {
		return fmt.Errorf("plugin %s: listen on %s: %w", cfg.Name, socketPath, err)
	}

	// Graceful shutdown on host-sent termination signals.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sigCh
		srv.GracefulStop()
	}()

	// The one and only stdout line; the host blocks on reading it (§5.2).
	fmt.Println(FormatHandshake(socketPath))
	_ = os.Stdout.Sync()

	err = srv.Serve(lis)
	_ = os.Remove(socketPath)
	return err
}

func registerKind(srv *grpc.Server, cfg ServeConfig) error {
	switch cfg.Kind {
	case pluginv1.PluginKind_PLUGIN_KIND_TARGET:
		if cfg.Target == nil {
			return fmt.Errorf("plugin %s: kind target requires a Target service", cfg.Name)
		}
		pluginv1.RegisterTargetServer(srv, cfg.Target)
	case pluginv1.PluginKind_PLUGIN_KIND_LOADDRIVER:
		if cfg.LoadDriver == nil {
			return fmt.Errorf("plugin %s: kind loaddriver requires a LoadDriver service", cfg.Name)
		}
		pluginv1.RegisterLoadDriverServer(srv, cfg.LoadDriver)
	case pluginv1.PluginKind_PLUGIN_KIND_PROVIDER:
		if cfg.Provider == nil {
			return fmt.Errorf("plugin %s: kind provider requires a Provider service", cfg.Name)
		}
		pluginv1.RegisterProviderServer(srv, cfg.Provider)
	case pluginv1.PluginKind_PLUGIN_KIND_STRATEGY:
		if cfg.Strategy == nil {
			return fmt.Errorf("plugin %s: kind strategy requires a Strategy service", cfg.Name)
		}
		pluginv1.RegisterStrategyServer(srv, cfg.Strategy)
	case pluginv1.PluginKind_PLUGIN_KIND_SCENARIO:
		if cfg.Scenario == nil {
			return fmt.Errorf("plugin %s: kind scenario requires a Scenario service", cfg.Name)
		}
		pluginv1.RegisterScenarioServer(srv, cfg.Scenario)
	case pluginv1.PluginKind_PLUGIN_KIND_EXPORTER:
		if cfg.Exporter == nil {
			return fmt.Errorf("plugin %s: kind exporter requires an Exporter service", cfg.Name)
		}
		pluginv1.RegisterExporterServer(srv, cfg.Exporter)
	case pluginv1.PluginKind_PLUGIN_KIND_CAPTURE:
		if cfg.Capture == nil {
			return fmt.Errorf("plugin %s: kind capture requires a Capture service", cfg.Name)
		}
		pluginv1.RegisterCaptureServer(srv, cfg.Capture)
	default:
		return fmt.Errorf("plugin %s: unspecified or unknown kind %v", cfg.Name, cfg.Kind)
	}
	return nil
}

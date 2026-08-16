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

package controller

import (
	"context"
	"fmt"

	"google.golang.org/grpc"

	"github.com/aburan28/parallax/internal/pluginhost"
	pluginv1 "github.com/aburan28/parallax/proto/plugin/v1"
)

// trialPlugins is the plugin call surface the Trial controller drives. It is backed
// by the plugin host in production (hostPlugins) and by a fake in tests, so the
// reconciler's state machine and collection logic are unit-testable without launching
// real subprocesses.
type trialPlugins interface {
	Apply(ctx context.Context, target string, req *pluginv1.ApplyRequest) (*pluginv1.ApplyResponse, error)
	Ready(ctx context.Context, target string, req *pluginv1.ReadyRequest) (*pluginv1.ReadyResponse, error)
	StartLoad(ctx context.Context, driver string, req *pluginv1.StartRequest) (*pluginv1.StartResponse, error)
	Progress(ctx context.Context, driver string, req *pluginv1.ProgressRequest) (*pluginv1.ProgressResponse, error)
	StopLoad(ctx context.Context, driver string, req *pluginv1.StopRequest) (*pluginv1.StopResponse, error)
	Collect(ctx context.Context, provider string, req *pluginv1.CollectRequest) (*pluginv1.CollectResponse, error)
}

// hostPlugins adapts pluginhost.Host to trialPlugins by dialing each plugin and
// building the typed gRPC client per call. Host.Conn returns a *grpc.ClientConn,
// which satisfies the grpc.ClientConnInterface the generated clients accept.
type hostPlugins struct {
	host pluginhost.Host
}

func (h hostPlugins) Apply(ctx context.Context, target string, req *pluginv1.ApplyRequest) (*pluginv1.ApplyResponse, error) {
	conn, err := h.dial(target)
	if err != nil {
		return nil, err
	}
	return pluginv1.NewTargetClient(conn).Apply(ctx, req)
}

func (h hostPlugins) Ready(ctx context.Context, target string, req *pluginv1.ReadyRequest) (*pluginv1.ReadyResponse, error) {
	conn, err := h.dial(target)
	if err != nil {
		return nil, err
	}
	return pluginv1.NewTargetClient(conn).Ready(ctx, req)
}

func (h hostPlugins) StartLoad(ctx context.Context, driver string, req *pluginv1.StartRequest) (*pluginv1.StartResponse, error) {
	conn, err := h.dial(driver)
	if err != nil {
		return nil, err
	}
	return pluginv1.NewLoadDriverClient(conn).Start(ctx, req)
}

func (h hostPlugins) Progress(ctx context.Context, driver string, req *pluginv1.ProgressRequest) (*pluginv1.ProgressResponse, error) {
	conn, err := h.dial(driver)
	if err != nil {
		return nil, err
	}
	return pluginv1.NewLoadDriverClient(conn).Progress(ctx, req)
}

func (h hostPlugins) StopLoad(ctx context.Context, driver string, req *pluginv1.StopRequest) (*pluginv1.StopResponse, error) {
	conn, err := h.dial(driver)
	if err != nil {
		return nil, err
	}
	return pluginv1.NewLoadDriverClient(conn).Stop(ctx, req)
}

func (h hostPlugins) Collect(ctx context.Context, provider string, req *pluginv1.CollectRequest) (*pluginv1.CollectResponse, error) {
	conn, err := h.dial(provider)
	if err != nil {
		return nil, err
	}
	return pluginv1.NewProviderClient(conn).Collect(ctx, req)
}

func (h hostPlugins) dial(name string) (grpc.ClientConnInterface, error) {
	if h.host == nil {
		return nil, fmt.Errorf("no plugin host configured")
	}
	conn, err := h.host.Conn(name)
	if err != nil {
		return nil, fmt.Errorf("plugin %q: %w", name, err)
	}
	return conn, nil
}

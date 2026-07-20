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
	"context"

	"github.com/aburan28/parallax/internal/version"
	pluginv1 "github.com/aburan28/parallax/proto/plugin/v1"
)

// BaseLifecycle is a ready-to-embed implementation of the Lifecycle service that
// handles ABI negotiation and build stamping. Plugin authors set the fields and
// optionally supply ConfigureFunc/HealthFunc/DrainFunc for custom behavior.
type BaseLifecycle struct {
	pluginv1.UnimplementedLifecycleServer

	Name         string
	Kind         pluginv1.PluginKind
	ConfigSchema string
	Capabilities []string

	// ConfigureFunc validates a config block; nil ⇒ accept everything.
	ConfigureFunc func(configJSON []byte) (accepted bool, reason string)
	// HealthFunc reports liveness; nil ⇒ always ok.
	HealthFunc func() (ok bool, detail string)
	// DrainFunc finishes in-flight work; nil ⇒ no-op. Must be idempotent.
	DrainFunc func() error
}

var _ pluginv1.LifecycleServer = (*BaseLifecycle)(nil)

func (b *BaseLifecycle) Describe(_ context.Context, req *pluginv1.DescribeRequest) (*pluginv1.DescribeResponse, error) {
	return &pluginv1.DescribeResponse{
		Name:               b.Name,
		Kind:               b.Kind,
		AbiVersions:        []string{ABIVersion},
		SelectedAbiVersion: negotiate(req.GetHostAbiVersions()),
		ConfigSchema:       b.ConfigSchema,
		Capabilities:       b.Capabilities,
		Build: &pluginv1.BuildInfo{
			Version: version.Version,
			VcsRef:  version.VCSRef,
		},
	}, nil
}

func (b *BaseLifecycle) Configure(_ context.Context, req *pluginv1.ConfigureRequest) (*pluginv1.ConfigureResponse, error) {
	if b.ConfigureFunc == nil {
		return &pluginv1.ConfigureResponse{Accepted: true}, nil
	}
	ok, reason := b.ConfigureFunc(req.GetConfigJson())
	return &pluginv1.ConfigureResponse{Accepted: ok, Reason: reason}, nil
}

func (b *BaseLifecycle) Health(_ context.Context, _ *pluginv1.HealthRequest) (*pluginv1.HealthResponse, error) {
	if b.HealthFunc == nil {
		return &pluginv1.HealthResponse{Ok: true}, nil
	}
	ok, detail := b.HealthFunc()
	return &pluginv1.HealthResponse{Ok: ok, Detail: detail}, nil
}

func (b *BaseLifecycle) Drain(_ context.Context, _ *pluginv1.DrainRequest) (*pluginv1.DrainResponse, error) {
	if b.DrainFunc != nil {
		if err := b.DrainFunc(); err != nil {
			return nil, err
		}
	}
	return &pluginv1.DrainResponse{}, nil
}

// negotiate picks this SDK's ABI version if the host supports it (N-1, §5.2).
func negotiate(hostVersions []string) string {
	if len(hostVersions) == 0 {
		return ABIVersion
	}
	for _, v := range hostVersions {
		if v == ABIVersion {
			return ABIVersion
		}
	}
	return "" // no overlap; host will surface a version mismatch
}

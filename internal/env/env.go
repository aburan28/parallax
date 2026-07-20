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

// Package env describes the local kind bootstrap components that back --local
// mode (DESIGN.md §11.1, §16): kapture, Envoy Gateway, MinIO and prom-lite.
//
// The real driver is hack/env-up.sh (kapture-style). This package only enumerates
// what that script provisions so the CLI can list/validate components without
// shelling out yet.
package env

import (
	"context"
	"fmt"
)

// Component identifiers for the local bootstrap stack.
const (
	// ComponentKapture is the SUT/data-plane under test (capture-agent, hub/spoke).
	ComponentKapture = "kapture"
	// ComponentEnvoyGateway is the Gateway API implementation load flows through.
	ComponentEnvoyGateway = "envoy-gateway"
	// ComponentMinIO is the S3-compatible capture + artifact storage.
	ComponentMinIO = "minio"
	// ComponentPromLite is the single-Prometheus profile with generated static
	// scrape configs (default for kind/CI).
	ComponentPromLite = "prom-lite"
)

// Component is one piece of the local bootstrap stack.
type Component struct {
	// Name is the stable identifier (one of the Component* constants).
	Name string
	// Description is a short human summary.
	Description string
	// Namespace is where the component installs.
	Namespace string
	// Optional marks components not required for the minimal path.
	Optional bool
}

// Components returns the ordered set of components hack/env-up.sh provisions for
// --local mode.
func Components() []Component {
	return []Component{
		{
			Name:        ComponentKapture,
			Description: "kapture capture-agent + hub/spoke (the system under test)",
			Namespace:   "capture-system",
			Optional:    false,
		},
		{
			Name:        ComponentEnvoyGateway,
			Description: "Envoy Gateway (Gateway API) — the mirror/load ingress path",
			Namespace:   "envoy-gateway-system",
			Optional:    false,
		},
		{
			Name:        ComponentMinIO,
			Description: "MinIO — S3-compatible capture and artifact storage",
			Namespace:   "minio",
			Optional:    false,
		},
		{
			Name:        ComponentPromLite,
			Description: "prom-lite — single Prometheus with generated static scrape configs",
			Namespace:   "monitoring",
			Optional:    false,
		},
	}
}

// Up provisions the local environment.
//
// TODO(m1): shell out to hack/env-up.sh (idempotent, profile-selectable), stream
// its output, and wait for each Component to become Ready. Returns an error in
// M0 so callers do not assume an environment exists.
func Up(ctx context.Context) error {
	_ = ctx
	return fmt.Errorf("env: Up not implemented; run hack/env-up.sh to provision the local stack")
}

// Down tears the local environment down.
//
// TODO(m1): shell out to hack/env-down.sh.
func Down(ctx context.Context) error {
	_ = ctx
	return fmt.Errorf("env: Down not implemented; run hack/env-down.sh to tear down the local stack")
}

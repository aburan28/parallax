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

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime"
)

// LocalRef references an object by name in the same namespace.
type LocalRef struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// SecretRef references a Secret by name (e.g. a benchmark-cluster kubeconfig).
type SecretRef struct {
	// +kubebuilder:validation:MinLength=1
	SecretName string `json:"secretName"`
}

// PluginRef names a Plugin CR and carries a config block validated by that
// plugin's Describe() JSON Schema (DESIGN.md §5.3).
type PluginRef struct {
	// +kubebuilder:validation:MinLength=1
	Plugin string `json:"plugin"`
	// +optional
	Config *runtime.RawExtension `json:"config,omitempty"`
}

// Numeric thresholds and bounds are carried as decimal strings in the CRD API to
// avoid Kubernetes' discouraged float fields; they are parsed by internal/analysis.
// +kubebuilder:validation:Pattern=`^-?[0-9]+(\.[0-9]+)?%?$`
type DecimalString string

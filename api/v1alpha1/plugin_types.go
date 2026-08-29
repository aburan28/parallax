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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// PluginKind is the extension seam a Plugin implements (DESIGN.md §5.1).
// +kubebuilder:validation:Enum=target;loaddriver;provider;strategy;scenario;exporter;capture
type PluginKind string

const (
	PluginKindTarget     PluginKind = "target"
	PluginKindLoadDriver PluginKind = "loaddriver"
	PluginKindProvider   PluginKind = "provider"
	PluginKindStrategy   PluginKind = "strategy"
	PluginKindScenario   PluginKind = "scenario"
	PluginKindExporter   PluginKind = "exporter"
	PluginKindCapture    PluginKind = "capture"
)

// PluginPhase tracks install/verify state (DESIGN.md §5.3).
type PluginPhase string

const (
	PluginPhasePending    PluginPhase = "Pending"
	PluginPhaseVerifying  PluginPhase = "Verifying"
	PluginPhaseInstalling PluginPhase = "Installing"
	PluginPhaseReady      PluginPhase = "Ready"
	PluginPhaseFailed     PluginPhase = "Failed"
	PluginPhaseDegraded   PluginPhase = "Degraded"
)

type PluginSpec struct {
	Kind PluginKind `json:"kind"`
	// OCI image (tag or digest) delivering the parallax-<kind>-<name> binary.
	// +kubebuilder:validation:MinLength=1
	Image string `json:"image"`
	// +optional
	Verify VerifySpec `json:"verify,omitempty"`
	// Default config, schema-validated via the plugin's Describe().
	// +optional
	Config *runtime.RawExtension `json:"config,omitempty"`
	// namespaceSelector gates which namespaces' Studies may use this plugin
	// (empty = all). DESIGN.md §5.3, §17.4.
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`
}

type VerifySpec struct {
	// +optional
	Cosign *CosignSpec `json:"cosign,omitempty"`
	// requireDigestPin forces @sha256 image references (org policy). §5.3.
	// +optional
	RequireDigestPin bool `json:"requireDigestPin,omitempty"`
}

type CosignSpec struct {
	// +optional
	KeylessIdentity string `json:"keylessIdentity,omitempty"`
	// +optional
	Rekor bool `json:"rekor,omitempty"`
	// PublicKeySecretRef enables offline verification (air-gap). §17.7.
	// +optional
	PublicKeySecretRef *SecretRef `json:"publicKeySecretRef,omitempty"`
}

type PluginStatus struct {
	// +optional
	Phase PluginPhase `json:"phase,omitempty"`
	// +optional
	ResolvedDigest string `json:"resolvedDigest,omitempty"`
	// +optional
	ABIVersion string `json:"abiVersion,omitempty"`
	// Operator replicas that have the binary installed.
	// +optional
	InstalledOn []string `json:"installedOn,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=pxplugin
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.kind`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Digest",type=string,JSONPath=`.status.resolvedDigest`
// +kubebuilder:printcolumn:name="ABI",type=string,JSONPath=`.status.abiVersion`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Plugin registers a gRPC subprocess plugin (DESIGN.md §5.3). Cluster-scoped.
type Plugin struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PluginSpec   `json:"spec,omitempty"`
	Status PluginStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PluginList contains a list of Plugin.
type PluginList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Plugin `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Plugin{}, &PluginList{})
}

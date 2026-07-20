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

// DatasetPhase tracks verification state (DESIGN.md §10).
type DatasetPhase string

const (
	DatasetPhasePending  DatasetPhase = "Pending"
	DatasetPhaseVerified DatasetPhase = "Verified"
	DatasetPhaseFailed   DatasetPhase = "Failed"
)

type DatasetSpec struct {
	// CaptureRef names the kapture TrafficCapture this dataset was recorded from.
	CaptureRef LocalRef `json:"captureRef"`
	StorageRef LocalRef `json:"storageRef"`
	// +optional
	Preshard *PreshardSpec `json:"preshard,omitempty"`
}

type PreshardSpec struct {
	// +kubebuilder:validation:Minimum=1
	Shards int32 `json:"shards"`
}

type DatasetStatus struct {
	// +optional
	Phase DatasetPhase `json:"phase,omitempty"`
	// Content digest stamped by the Dataset controller; used by readback fidelity SLIs.
	// +optional
	IDDigest string `json:"idDigest,omitempty"`
	// +optional
	Manifest *runtime.RawExtension `json:"manifest,omitempty"`
	// +optional
	RecordCount int64 `json:"recordCount,omitempty"`
	// +optional
	VerifiedAt *metav1.Time `json:"verifiedAt,omitempty"`
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
// +kubebuilder:resource:scope=Namespaced,shortName=pxds
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Records",type=integer,JSONPath=`.status.recordCount`
// +kubebuilder:printcolumn:name="Digest",type=string,JSONPath=`.status.idDigest`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Dataset is the Schema for the datasets API: a registered, verified workload.
type Dataset struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DatasetSpec   `json:"spec,omitempty"`
	Status DatasetStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DatasetList contains a list of Dataset.
type DatasetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Dataset `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Dataset{}, &DatasetList{})
}

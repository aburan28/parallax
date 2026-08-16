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

// TrialPhase is the trial state-machine phase (DESIGN.md §9).
type TrialPhase string

const (
	TrialPhasePending     TrialPhase = "Pending"
	TrialPhaseConfiguring TrialPhase = "Configuring"
	TrialPhaseHealthGate  TrialPhase = "HealthGate"
	TrialPhaseWarmup      TrialPhase = "Warmup"
	TrialPhaseMeasuring   TrialPhase = "Measuring"
	TrialPhaseDraining    TrialPhase = "Draining"
	TrialPhaseCollecting  TrialPhase = "Collecting"
	TrialPhaseCollected   TrialPhase = "Collected"
	TrialPhaseFailed      TrialPhase = "Failed"
	TrialPhaseAborted     TrialPhase = "Aborted"
	TrialPhaseInvalid     TrialPhase = "Invalid"
)

// TrialMode distinguishes screening from validation reps and scenario reps.
type TrialMode string

const (
	TrialModeScreening  TrialMode = "screening"
	TrialModeValidation TrialMode = "validation"
	TrialModeScenario   TrialMode = "scenario"
)

// TrialSpec is the resolved (config point, workload, fidelity, rep, mode).
type TrialSpec struct {
	StudyRef LocalRef `json:"studyRef"`
	// +kubebuilder:validation:MinLength=1
	ConfigHash string `json:"configHash"`
	// +optional
	Config *runtime.RawExtension `json:"config,omitempty"`
	// Dimension path → resolved value for this point.
	// +optional
	Dimensions map[string]string `json:"dimensions,omitempty"`
	// +kubebuilder:validation:MinLength=1
	Workload string `json:"workload"`
	// +optional
	Fidelity FidelitySpec `json:"fidelity,omitempty"`
	// +optional
	Rep int32 `json:"rep,omitempty"`
	// +optional
	Mode TrialMode `json:"mode,omitempty"`
	// +optional
	Scenario string `json:"scenario,omitempty"`
}

// FidelitySpec is the dial that separates cheap screening trials from expensive
// validation trials (DESIGN.md §7 fidelity note). Durations are core semantics;
// intensity is driver-specific, so it rides along as a config overlay.
type FidelitySpec struct {
	// +optional
	Warmup metav1.Duration `json:"warmup,omitempty"`
	// +optional
	Measure metav1.Duration `json:"measure,omitempty"`
	// DriverConfig overlays the workload's driver config for this trial — a lower
	// request rate, a smaller scale factor, fewer client threads. Merged over
	// Workload.Driver.Config; driver-defined, opaque to the core.
	// +optional
	DriverConfig *runtime.RawExtension `json:"driverConfig,omitempty"`
}

// TrialTimeline records the phase boundary timestamps; SLIs are evaluated strictly
// inside [T1, T2] (DESIGN.md §9, "timestamps are law").
type TrialTimeline struct {
	// +optional
	Applied *metav1.Time `json:"applied,omitempty"`
	// +optional
	Ready *metav1.Time `json:"ready,omitempty"`
	// LoadStarted is when the load driver's run began. Warmup is measured from here,
	// not from Ready, so a slow driver start does not eat the warmup.
	// +optional
	LoadStarted *metav1.Time `json:"loadStarted,omitempty"`
	// +optional
	T1 *metav1.Time `json:"t1,omitempty"`
	// +optional
	T2 *metav1.Time `json:"t2,omitempty"`
	// +optional
	Drained *metav1.Time `json:"drained,omitempty"`
}

// LoadRunStatus is the observed state of the trial's load-driver run.
type LoadRunStatus struct {
	// RunRef is the opaque handle the driver returned from Start; the core hands it
	// back on every Progress/Stop call and never assumes its shape.
	// +optional
	RunRef string `json:"runRef,omitempty"`
	// Metrics are the driver's summary metrics from Stop, as decimal strings (CRD
	// float avoidance). SLIs with a `driver` source read from this map.
	// +optional
	Metrics map[string]string `json:"metrics,omitempty"`
	// Aborted records that the driver's own abort policy fired.
	// +optional
	Aborted bool `json:"aborted,omitempty"`
	// +optional
	AbortReason string `json:"abortReason,omitempty"`
}

// SLIResult is a status-level summary of one SLI; full evidence lives in the DB.
type SLIResult struct {
	Name string `json:"name"`
	// +optional
	Class string `json:"class,omitempty"`
	Value string `json:"value"`
}

type GuardrailSummary struct {
	// +optional
	Passed int32 `json:"passed,omitempty"`
	// +optional
	Failed int32 `json:"failed,omitempty"`
}

// RunRef ties a Trial CR to its rows in the results DB.
type RunRef struct {
	// +optional
	DB string `json:"db,omitempty"`
	// +optional
	RunID string `json:"runID,omitempty"`
	// +optional
	TrialID string `json:"trialID,omitempty"`
}

type TrialStatus struct {
	// +optional
	Phase TrialPhase `json:"phase,omitempty"`
	// +optional
	Timeline TrialTimeline `json:"timeline,omitempty"`
	// Environment fingerprint (k8s version, plugin digests, images) — §9.
	// +optional
	Fingerprint *runtime.RawExtension `json:"fingerprint,omitempty"`
	// +optional
	Load LoadRunStatus `json:"load,omitempty"`
	// +optional
	SLIs []SLIResult `json:"slis,omitempty"`
	// +optional
	Guardrails GuardrailSummary `json:"guardrails,omitempty"`
	// +optional
	Run RunRef `json:"run,omitempty"`
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
// +kubebuilder:resource:scope=Namespaced,shortName=trial
// +kubebuilder:printcolumn:name="Study",type=string,JSONPath=`.spec.studyRef.name`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Config",type=string,JSONPath=`.spec.configHash`
// +kubebuilder:printcolumn:name="Rep",type=integer,JSONPath=`.spec.rep`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Trial is the Schema for the trials API. High-cardinality by design: GC'd by owner
// deletion, full data in the results DB (DESIGN.md §6).
type Trial struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TrialSpec   `json:"spec,omitempty"`
	Status TrialStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// TrialList contains a list of Trial.
type TrialList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Trial `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Trial{}, &TrialList{})
}

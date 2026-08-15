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

// StudyPhase is the funnel phase of a Study (DESIGN.md §4.1, §7).
type StudyPhase string

const (
	StudyPhasePending          StudyPhase = "Pending"
	StudyPhaseSweeping         StudyPhase = "Sweeping"
	StudyPhaseSelecting        StudyPhase = "Selecting"
	StudyPhaseValidating       StudyPhase = "Validating"
	StudyPhaseAwaitingApproval StudyPhase = "AwaitingApproval"
	StudyPhasePromoted         StudyPhase = "Promoted"
	StudyPhaseRejected         StudyPhase = "Rejected"
	StudyPhaseFailed           StudyPhase = "Failed"
)

// StudySpec is the declarative experiment (DESIGN.md §8).
type StudySpec struct {
	// Target names a Ready Plugin of kind target and its config.
	Target PluginRef `json:"target"`

	// +optional
	Environment EnvironmentSpec `json:"environment,omitempty"`

	// Baseline is the reference configuration every candidate is compared against.
	// +optional
	Baseline BaselineSpec `json:"baseline,omitempty"`

	// Space is the search space (dimensions, constraints, strategy).
	Space SpaceSpec `json:"space"`

	// +kubebuilder:validation:MinItems=1
	Workloads []Workload `json:"workloads"`

	// +kubebuilder:validation:MinItems=1
	SLIs []SLISpec `json:"slis"`

	// +optional
	Guardrails []Guardrail `json:"guardrails,omitempty"`

	Objectives ObjectivesSpec `json:"objectives"`

	// +optional
	Selection SelectionSpec `json:"selection,omitempty"`

	// +optional
	Validation ValidationSpec `json:"validation,omitempty"`

	// +optional
	Budgets BudgetSpec `json:"budgets,omitempty"`

	// +optional
	Promotion PromotionSpec `json:"promotion,omitempty"`

	// Schedule, when set, re-runs the study on a cron cadence appending to the same
	// DB lineage (DESIGN.md §16). Format: standard cron.
	// +optional
	Schedule string `json:"schedule,omitempty"`
}

type EnvironmentSpec struct {
	// ClusterRef selects a benchmark cluster kubeconfig secret; omit = operator's cluster.
	// +optional
	ClusterRef *SecretRef `json:"clusterRef,omitempty"`
}

type BaselineSpec struct {
	// +optional
	Name string `json:"name,omitempty"`
	// +optional
	Values *runtime.RawExtension `json:"values,omitempty"`
}

type SpaceSpec struct {
	// +kubebuilder:validation:MinItems=1
	Dimensions []Dimension `json:"dimensions"`
	// CEL expressions pruning invalid combinations before trials are spent (§8).
	// +optional
	Constraints []string  `json:"constraints,omitempty"`
	Strategy    PluginRef `json:"strategy"`
}

// Dimension is one axis of the search space. Exactly one of Int/Float/Categorical
// must be set.
type Dimension struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
	// Path is the target-plugin config location this dimension writes: a Helm value
	// path (`.env.GOGC`), a JSON pointer, a flag name — whatever the target's
	// Contract() accepts. Defaults to Name, preserving the "dimension names are
	// target paths" convention target-kapture relies on (DESIGN.md §8).
	// +optional
	Path string `json:"path,omitempty"`
	// Mapping carries a richer target-defined mapping for dimensions a single Path
	// cannot express (e.g. one axis fanning out to several fields). Opaque to the
	// core; validated by the target plugin.
	// +optional
	Mapping *runtime.RawExtension `json:"mapping,omitempty"`
	// +optional
	Int *IntRange `json:"int,omitempty"`
	// +optional
	Float *FloatRange `json:"float,omitempty"`
	// +optional
	Categorical *CategoricalValues `json:"categorical,omitempty"`
}

type IntRange struct {
	Min int64 `json:"min"`
	Max int64 `json:"max"`
	// +optional
	Log bool `json:"log,omitempty"`
}

// FloatRange bounds are decimal strings (see DecimalString) to avoid CRD floats.
type FloatRange struct {
	Min DecimalString `json:"min"`
	Max DecimalString `json:"max"`
	// +optional
	Log bool `json:"log,omitempty"`
}

type CategoricalValues struct {
	// +kubebuilder:validation:MinItems=1
	Values []string `json:"values"`
}

// CompletionMode decides what closes a workload's measurement window.
const (
	// CompletionDuration measures a fixed window: the workload is continuous and the
	// study decides how long to watch it (steady-state load, a running service).
	CompletionDuration = "duration"
	// CompletionDriver lets the load driver end the window by reporting done: the
	// workload runs to completion and its natural length *is* the measurement
	// (a batch job, a fixed-size corpus, a benchmark suite, an eval set).
	CompletionDriver = "driver"
)

// Workload is one load definition: a driver plugin, its driver-defined config, and
// the measurement window around it. The core knows nothing about what the driver
// does — replay, synthetic request generation, a batch submission, a query harness
// (DESIGN.md §8; docs/GENERALIZATION.md G1).
type Workload struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Driver names a Ready Plugin of kind loaddriver plus its config. The config is
	// opaque to parallax and validated by the plugin's Describe() JSON Schema — a
	// replay driver takes rate/distribution/abort here, a batch driver takes a job
	// template, a query driver takes a scale factor.
	Driver PluginRef `json:"driver"`

	// DatasetRef names the corpus this workload replays or reads. Optional: drivers
	// that generate load synthetically have no dataset.
	// +optional
	DatasetRef *LocalRef `json:"datasetRef,omitempty"`

	// Completion selects what ends the measurement window (see CompletionMode).
	// +optional
	// +kubebuilder:validation:Enum=duration;driver
	// +kubebuilder:default=duration
	Completion string `json:"completion,omitempty"`

	// Warmup runs the load unmeasured before t1 is stamped.
	// +optional
	Warmup metav1.Duration `json:"warmup,omitempty"`

	// Measure is the measurement window for completion=duration. Ignored when
	// completion=driver, where the driver decides and MaxDuration bounds it.
	// Defaulted so an omitted window can never silently mean "measure nothing".
	// +optional
	// +kubebuilder:default="5m"
	Measure metav1.Duration `json:"measure,omitempty"`

	// MaxDuration caps the measurement window regardless of mode: a driver that
	// never reports done, or a run that overshoots, aborts the trial rather than
	// burning the study's wall-clock budget.
	// +optional
	MaxDuration metav1.Duration `json:"maxDuration,omitempty"`

	// Cooldown quiesces the system after the window closes, before collection.
	// +optional
	Cooldown metav1.Duration `json:"cooldown,omitempty"`
}

// SLISpec declares one service-level indicator and where its value comes from.
// Exactly one of From/Driver/Derived must be set.
type SLISpec struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`

	// Trust class: fidelity | client | server | derived (§11).
	// +optional
	Class string `json:"class,omitempty"`

	// From evaluates the SLI through a provider plugin. Config is the provider's own
	// query dialect — `{query: <PromQL>}` for prometheus, whatever a third-party
	// provider declares. The core treats it as opaque (docs/GENERALIZATION.md G6).
	// +optional
	From *PluginRef `json:"from,omitempty"`

	// Driver reads a metric the workload's load driver reported when the run ended.
	// The core already holds these (StopResponse.metrics), so no plugin round-trip is
	// involved — and any driver's metric names work, not just an HTTP replay's.
	// +optional
	Driver *DriverMetric `json:"driver,omitempty"`

	// Derived computes the SLI in-core from other SLIs of the same trial; no plugin
	// is involved.
	// +optional
	Derived *DerivedSLI `json:"derived,omitempty"`
}

// DriverMetric names one key of the load driver's reported metrics map.
type DriverMetric struct {
	// +kubebuilder:validation:MinLength=1
	Metric string `json:"metric"`
}

// DerivedSLI is an arithmetic expression over other SLI names in the same study.
type DerivedSLI struct {
	// +kubebuilder:validation:MinLength=1
	Expr string `json:"expr"`
}

// Guardrail is a hard constraint; a breach disqualifies a config (DESIGN.md §8).
type Guardrail struct {
	// +optional
	SLI string `json:"sli,omitempty"`
	// +optional
	Max *DecimalString `json:"max,omitempty"`
	// +optional
	Min *DecimalString `json:"min,omitempty"`
	// +optional
	MaxRelativeToBaseline *DecimalString `json:"maxRelativeToBaseline,omitempty"`
	// From is the inline provider form: a guardrail evaluated directly, without a
	// named SLI. Same shape as SLISpec.From.
	// +optional
	From *PluginRef `json:"from,omitempty"`
}

type ObjectivesSpec struct {
	Primary Objective `json:"primary"`
	// +optional
	Secondary []Objective `json:"secondary,omitempty"`
}

type Objective struct {
	// +kubebuilder:validation:MinLength=1
	SLI string `json:"sli"`
	// +kubebuilder:validation:Enum=minimize;maximize
	Direction string `json:"direction"`
	// +optional
	MinPracticalEffect DecimalString `json:"minPracticalEffect,omitempty"`
}

type SelectionSpec struct {
	// +optional
	// +kubebuilder:default=3
	TopK int32 `json:"topK,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=pareto-weighted;lexicographic
	Method string `json:"method,omitempty"`
}

type ValidationSpec struct {
	// +optional
	// +kubebuilder:default=5
	Reps int32 `json:"reps,omitempty"`
	// +optional
	Alpha DecimalString `json:"alpha,omitempty"`
	// +optional
	// +kubebuilder:validation:Enum=holm;bonferroni;none
	Correction string `json:"correction,omitempty"`
	// +optional
	Scenarios []string `json:"scenarios,omitempty"`
}

type BudgetSpec struct {
	// +optional
	MaxTrials int32 `json:"maxTrials,omitempty"`
	// +optional
	MaxClusterTime metav1.Duration `json:"maxClusterTime,omitempty"`
	// +optional
	MaxWallClock metav1.Duration `json:"maxWallClock,omitempty"`
}

type PromotionSpec struct {
	// +optional
	// +kubebuilder:validation:Enum=manual;auto
	Approval string `json:"approval,omitempty"`
	// +optional
	Output string `json:"output,omitempty"`
	// +optional
	Require []string `json:"require,omitempty"`
}

// StudyStatus is the observed funnel state (DESIGN.md §6).
type StudyStatus struct {
	// +optional
	Phase StudyPhase `json:"phase,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	// +patchMergeKey=type
	// +patchStrategy=merge
	Conditions []metav1.Condition `json:"conditions,omitempty" patchStrategy:"merge" patchMergeKey:"type"`
	// +optional
	TrialsTotal int32 `json:"trialsTotal,omitempty"`
	// +optional
	TrialsCompleted int32 `json:"trialsCompleted,omitempty"`
	// Candidate config hashes advanced to validation.
	// +optional
	Candidates []string `json:"candidates,omitempty"`
	// RunID is the results-DB run this study is writing to.
	// +optional
	RunID string `json:"runID,omitempty"`
	// +optional
	DecisionRef string `json:"decisionRef,omitempty"`
	// +optional
	PromotionRef string `json:"promotionRef,omitempty"`
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=study
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Trials",type=string,JSONPath=`.status.trialsCompleted`
// +kubebuilder:printcolumn:name="Run",type=string,JSONPath=`.status.runID`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Study is the Schema for the studies API.
type Study struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   StudySpec   `json:"spec,omitempty"`
	Status StudyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// StudyList contains a list of Study.
type StudyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Study `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Study{}, &StudyList{})
}

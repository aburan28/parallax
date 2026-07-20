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

package store

import (
	"encoding/json"
	"time"
)

// The records below mirror the results-DB schema (DESIGN.md §15). They are the
// storage projection of the CRD types — deliberately denormalized for SQL queries.
// jsonb columns are carried as json.RawMessage.

type StudyRecord struct {
	ID        int64           `json:"id"`
	Name      string          `json:"name"`
	Namespace string          `json:"namespace"`
	Spec      json.RawMessage `json:"spec"`
	SpecHash  string          `json:"specHash"`
	CreatedAt time.Time       `json:"createdAt"`
}

type RunRecord struct {
	ID          int64           `json:"id"`
	StudyID     int64           `json:"studyId"`
	Seed        int64           `json:"seed"`
	Fingerprint json.RawMessage `json:"fingerprint"`
	Phase       string          `json:"phase"`
	StartedAt   *time.Time      `json:"startedAt,omitempty"`
	CompletedAt *time.Time      `json:"completedAt,omitempty"`
}

type TrialRecord struct {
	ID         int64           `json:"id"`
	RunID      int64           `json:"runId"`
	ConfigHash string          `json:"configHash"`
	Config     json.RawMessage `json:"config"`
	Workload   string          `json:"workload"`
	Fidelity   json.RawMessage `json:"fidelity"`
	Rep        int             `json:"rep"`
	Mode       string          `json:"mode"`
	Phase      string          `json:"phase"`
	Validity   string          `json:"validity"`
	Timeline   json.RawMessage `json:"timeline"`
	CreatedAt  time.Time       `json:"createdAt"`
}

// SLIValueRecord is the narrow fact table everything joins on (DESIGN.md §15).
type SLIValueRecord struct {
	ID          int64     `json:"id"`
	TrialID     int64     `json:"trialId"`
	Name        string    `json:"name"`
	Provider    string    `json:"provider"`
	Class       string    `json:"class"`
	Value       float64   `json:"value"`
	Query       string    `json:"query"`
	EvaluatedAt time.Time `json:"evaluatedAt"`
}

// ObservationRecord is the search log used for anytime curves.
type ObservationRecord struct {
	RunID          int64           `json:"runId"`
	Seq            int             `json:"seq"`
	ConfigHash     string          `json:"configHash"`
	Fidelity       json.RawMessage `json:"fidelity"`
	Objective      float64         `json:"objective"`
	BudgetCounters json.RawMessage `json:"budgetCounters"`
}

type DecisionRecord struct {
	ID        int64           `json:"id"`
	RunID     int64           `json:"runId"`
	Stage     string          `json:"stage"`
	Record    json.RawMessage `json:"record"`
	CreatedAt time.Time       `json:"createdAt"`
}

type ValidationRecord struct {
	RunID         int64           `json:"runId"`
	CandidateHash string          `json:"candidateHash"`
	Stats         json.RawMessage `json:"stats"`
}

type ScenarioRecord struct {
	RunID         int64           `json:"runId"`
	CandidateHash string          `json:"candidateHash"`
	Scenario      string          `json:"scenario"`
	Verdict       json.RawMessage `json:"verdict"`
}

type PromotionRecord struct {
	RunID       int64     `json:"runId"`
	ArtifactRef string    `json:"artifactRef"`
	Approver    string    `json:"approver"`
	ApprovedAt  time.Time `json:"approvedAt"`
}

type ArtifactRecord struct {
	TrialID int64  `json:"trialId"`
	Kind    string `json:"kind"`
	URI     string `json:"uri"`
	Digest  string `json:"digest"`
	Bytes   int64  `json:"bytes"`
}

type DatasetRecord struct {
	Name       string          `json:"name"`
	Manifest   json.RawMessage `json:"manifest"`
	IDDigest   string          `json:"idDigest"`
	VerifiedAt *time.Time      `json:"verifiedAt,omitempty"`
}

type PluginAuditRecord struct {
	Plugin string    `json:"plugin"`
	Kind   string    `json:"kind"`
	Digest string    `json:"digest"`
	Action string    `json:"action"` // installed | reloaded | failed
	Actor  string    `json:"actor"`
	At     time.Time `json:"at"`
}

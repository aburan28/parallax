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

package analysis

import (
	"testing"

	"github.com/aburan28/parallax/api/v1alpha1"
)

func decimal(s string) *v1alpha1.DecimalString {
	d := v1alpha1.DecimalString(s)
	return &d
}

// A single-objective study: minimize cost, with a hard loss guardrail. The lowest
// feasible cost must win; the guardrail-breaching config must be excluded.
func TestSelect_SingleObjectiveWithGuardrail(t *testing.T) {
	in := SelectInput{
		Objectives: v1alpha1.ObjectivesSpec{
			Primary: v1alpha1.Objective{SLI: "cost", Direction: "minimize"},
		},
		Guardrails: []v1alpha1.Guardrail{
			{SLI: "loss", Max: decimal("0")},
		},
		Selection: v1alpha1.SelectionSpec{TopK: 2},
		Baseline:  "base",
		Trials: []TrialData{
			{ConfigHash: "base", SLIs: map[string]float64{"cost": 100, "loss": 0}},
			{ConfigHash: "good", SLIs: map[string]float64{"cost": 60, "loss": 0}},
			{ConfigHash: "best", SLIs: map[string]float64{"cost": 40, "loss": 0}},
			{ConfigHash: "cheat", SLIs: map[string]float64{"cost": 10, "loss": 0.2}}, // breaches guardrail
		},
	}
	rec := Select(in)

	// "cheat" must be infeasible and never ranked.
	cheat := pointByHash(t, rec, "cheat")
	if cheat.Feasible {
		t.Fatalf("cheat breaches loss guardrail but was marked feasible")
	}
	if cheat.Rank != 0 || cheat.OnFront {
		t.Fatalf("infeasible cheat must not be on the front or ranked, got rank=%d front=%v", cheat.Rank, cheat.OnFront)
	}

	// "best" (lowest feasible cost) must rank #1.
	best := pointByHash(t, rec, "best")
	if best.Rank != 1 {
		t.Fatalf("best should rank 1, got %d", best.Rank)
	}

	// Candidates = top-2 (best, good) + baseline.
	if !contains(rec.Candidates, "best") || !contains(rec.Candidates, "good") {
		t.Fatalf("expected best+good among candidates, got %v", rec.Candidates)
	}
	if !contains(rec.Candidates, "base") {
		t.Fatalf("baseline must always advance, got %v", rec.Candidates)
	}
	if contains(rec.Candidates, "cheat") {
		t.Fatalf("infeasible config must not advance, got %v", rec.Candidates)
	}
}

// A two-objective study exercises the Pareto front: a config dominated on both
// objectives must be off the front; non-dominated trade-offs must be on it.
func TestSelect_ParetoTwoObjectives(t *testing.T) {
	in := SelectInput{
		Objectives: v1alpha1.ObjectivesSpec{
			Primary:   v1alpha1.Objective{SLI: "latency", Direction: "minimize"},
			Secondary: []v1alpha1.Objective{{SLI: "cpu", Direction: "minimize"}},
		},
		Selection: v1alpha1.SelectionSpec{TopK: 3},
		Trials: []TrialData{
			{ConfigHash: "a", SLIs: map[string]float64{"latency": 10, "cpu": 5}}, // trade-off
			{ConfigHash: "b", SLIs: map[string]float64{"latency": 5, "cpu": 10}}, // trade-off
			{ConfigHash: "c", SLIs: map[string]float64{"latency": 12, "cpu": 8}}, // dominated by a
		},
	}
	rec := Select(in)

	if !pointByHash(t, rec, "a").OnFront || !pointByHash(t, rec, "b").OnFront {
		t.Fatalf("a and b are non-dominated trade-offs and must be on the front")
	}
	if pointByHash(t, rec, "c").OnFront {
		t.Fatalf("c is dominated by a and must be off the front")
	}
}

// Selection must be deterministic: identical input yields identical candidates.
func TestSelect_Deterministic(t *testing.T) {
	in := SelectInput{
		Objectives: v1alpha1.ObjectivesSpec{Primary: v1alpha1.Objective{SLI: "x", Direction: "minimize"}},
		Selection:  v1alpha1.SelectionSpec{TopK: 2},
		Trials: []TrialData{
			{ConfigHash: "p", SLIs: map[string]float64{"x": 3}},
			{ConfigHash: "q", SLIs: map[string]float64{"x": 1}},
			{ConfigHash: "r", SLIs: map[string]float64{"x": 2}},
		},
	}
	a := Select(in)
	b := Select(in)
	if len(a.Candidates) != len(b.Candidates) {
		t.Fatalf("nondeterministic candidate count: %v vs %v", a.Candidates, b.Candidates)
	}
	for i := range a.Candidates {
		if a.Candidates[i] != b.Candidates[i] {
			t.Fatalf("nondeterministic ordering: %v vs %v", a.Candidates, b.Candidates)
		}
	}
	// q has the lowest x, so it must rank first.
	if a.Candidates[0] != "q" {
		t.Fatalf("expected q (lowest x) first, got %v", a.Candidates)
	}
}

// Reps of the same config are averaged before selection.
func TestSelect_AggregatesRepsByHash(t *testing.T) {
	in := SelectInput{
		Objectives: v1alpha1.ObjectivesSpec{Primary: v1alpha1.Objective{SLI: "m", Direction: "minimize"}},
		Selection:  v1alpha1.SelectionSpec{TopK: 1},
		Trials: []TrialData{
			{ConfigHash: "h", SLIs: map[string]float64{"m": 2}},
			{ConfigHash: "h", SLIs: map[string]float64{"m": 4}}, // mean = 3
		},
	}
	rec := Select(in)
	got := pointByHash(t, rec, "h").SLIs["m"]
	if got != 3 {
		t.Fatalf("expected averaged m=3, got %g", got)
	}
}

func pointByHash(t *testing.T, rec *DecisionRecord, hash string) PointDecision {
	t.Helper()
	for _, p := range rec.Points {
		if p.Hash == hash {
			return p
		}
	}
	t.Fatalf("no point for hash %q", hash)
	return PointDecision{}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

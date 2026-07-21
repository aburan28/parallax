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
	"sort"

	"github.com/aburan28/parallax/api/v1alpha1"
)

// Selection method values (matching the CRD SelectionSpec.Method enum).
const (
	MethodParetoWeighted = "pareto-weighted"
	MethodLexicographic  = "lexicographic"
)

// TrialData is one trial's contribution to selection: its config hash, mode,
// validity verdict, and the SLI values collected for it. It is intentionally
// store-agnostic so Select stays a pure function (DESIGN.md §12).
type TrialData struct {
	ConfigHash string
	Mode       string
	// Validity is "invalid" for instrument failures (excluded, §12); any other
	// value (including "") is treated as valid.
	Validity string
	SLIs     map[string]float64
}

// SelectInput is everything Select needs, lifted out of the Study spec + results.
type SelectInput struct {
	Objectives v1alpha1.ObjectivesSpec
	Guardrails []v1alpha1.Guardrail
	Selection  v1alpha1.SelectionSpec
	Trials     []TrialData
	// Baseline is the baseline config hash; it always advances to validation (§7).
	Baseline string
}

// GuardrailVerdict records one guardrail evaluation for the decision record.
type GuardrailVerdict struct {
	SLI    string  `json:"sli,omitempty"`
	Value  float64 `json:"value"`
	Pass   bool    `json:"pass"`
	Reason string  `json:"reason,omitempty"`
}

// PointDecision is the per-config decision detail — every SLI value, guardrail
// verdict, Pareto status, rank, and score (DESIGN.md §12).
type PointDecision struct {
	Hash        string             `json:"hash"`
	Valid       bool               `json:"valid"`
	Feasible    bool               `json:"feasible"`
	SLIs        map[string]float64 `json:"slis"`
	Guardrails  []GuardrailVerdict `json:"guardrails"`
	OnFront     bool               `json:"onParetoFront"`
	Rank        int                `json:"rank,omitempty"`
	Score       float64            `json:"score"`
	IsBaseline  bool               `json:"isBaseline"`
	IsCandidate bool               `json:"isCandidate"`
}

// DecisionRecord is the auditable output of selection, stored in the decisions
// table and re-derivable offline (DESIGN.md §12, §15).
type DecisionRecord struct {
	Stage      string             `json:"stage"`
	Method     string             `json:"method"`
	TopK       int                `json:"topK"`
	Objectives map[string]string  `json:"objectives"`
	Weights    map[string]float64 `json:"weights"`
	Baseline   string             `json:"baseline,omitempty"`
	Candidates []string           `json:"candidates"`
	Points     []PointDecision    `json:"points"`
}

// Select runs the selection funnel over trial results: aggregate by config →
// validity filter → guardrails → Pareto front → rank → top-K + baseline. It is a
// pure, deterministic function so `parallax select --run <id>` reproduces the same
// decision from the DB (DESIGN.md §12).
func Select(in SelectInput) *DecisionRecord {
	dirs, weights, objNames := objectivesOf(in.Objectives)

	agg := aggregateByHash(in.Trials)
	hashes := sortedKeys(agg)

	baseVals := agg[in.Baseline].slis // nil-safe map read below

	rec := &DecisionRecord{
		Stage:      "select",
		Method:     methodOrDefault(in.Selection.Method),
		TopK:       topKOrDefault(in.Selection.TopK),
		Objectives: dirs,
		Weights:    weights,
		Baseline:   in.Baseline,
		Candidates: []string{},
		Points:     make([]PointDecision, 0, len(hashes)),
	}

	// Build per-config decisions and the eligible-for-Pareto points.
	byHash := map[string]*PointDecision{}
	var eligible []Point
	for _, h := range hashes {
		a := agg[h]
		pd := PointDecision{
			Hash:       h,
			Valid:      a.valid,
			SLIs:       a.slis,
			IsBaseline: h == in.Baseline,
		}
		feasible := a.valid
		for _, g := range in.Guardrails {
			v := guardrailVerdict(g, a.slis, baseVals)
			pd.Guardrails = append(pd.Guardrails, v)
			if !v.Pass {
				feasible = false
			}
		}
		pd.Feasible = feasible

		objVals := make(map[string]float64, len(objNames))
		for _, name := range objNames {
			objVals[name] = a.slis[name]
		}
		pt := Point{Hash: h, Objectives: objVals, Guardrails: a.slis, Feasible: feasible && a.valid}
		pd.Score = WeightedScore(pt, weights, dirs)

		rec.Points = append(rec.Points, pd)
		byHash[h] = &rec.Points[len(rec.Points)-1]
		if pt.Feasible {
			eligible = append(eligible, pt)
		}
	}

	// Pareto front over the eligible points (recorded for provenance).
	front := ParetoFront(eligible, dirs)
	frontSet := map[string]bool{}
	for _, p := range front {
		frontSet[p.Hash] = true
		byHash[p.Hash].OnFront = true
	}
	// Rank ALL feasible points, front members first, then dominated-but-feasible by
	// score. Ranking the full feasible set (not just the front) is what lets top-K
	// advance K distinct configs even for single-objective studies, hedging screening
	// noise (DESIGN.md §20: "top-K (not top-1) advances").
	ranked := rankFeasible(eligible, frontSet, rec.Method, weights, dirs, in.Objectives)
	for i, h := range ranked {
		byHash[h].Rank = i + 1
	}

	// Top-K of the ranked front advance, plus the baseline (§7).
	seen := map[string]bool{}
	for i, h := range ranked {
		if i >= rec.TopK {
			break
		}
		byHash[h].IsCandidate = true
		rec.Candidates = append(rec.Candidates, h)
		seen[h] = true
	}
	if in.Baseline != "" && !seen[in.Baseline] {
		if pd, ok := byHash[in.Baseline]; ok {
			pd.IsCandidate = true
			rec.Candidates = append(rec.Candidates, in.Baseline)
		}
	}
	return rec
}

// aggregate holds the mean SLI values for one config hash across its trials.
type aggregate struct {
	slis  map[string]float64
	valid bool
}

// aggregateByHash averages each SLI across the trials sharing a config hash and
// marks a config valid if at least one of its trials was not an instrument failure.
func aggregateByHash(trials []TrialData) map[string]aggregate {
	sums := map[string]map[string]float64{}
	counts := map[string]map[string]int{}
	valid := map[string]bool{}
	for _, t := range trials {
		if t.ConfigHash == "" {
			continue
		}
		if _, ok := sums[t.ConfigHash]; !ok {
			sums[t.ConfigHash] = map[string]float64{}
			counts[t.ConfigHash] = map[string]int{}
		}
		if t.Validity != "invalid" {
			valid[t.ConfigHash] = true
		}
		for name, v := range t.SLIs {
			sums[t.ConfigHash][name] += v
			counts[t.ConfigHash][name]++
		}
	}
	out := make(map[string]aggregate, len(sums))
	for h, s := range sums {
		means := make(map[string]float64, len(s))
		for name, total := range s {
			means[name] = total / float64(counts[h][name])
		}
		out[h] = aggregate{slis: means, valid: valid[h]}
	}
	return out
}

// guardrailVerdict evaluates one guardrail against a config's SLI values. Inline
// provider-query guardrails (no named SLI) are deferred to live evaluation (m2).
func guardrailVerdict(g v1alpha1.Guardrail, slis, baseline map[string]float64) GuardrailVerdict {
	if g.SLI == "" {
		return GuardrailVerdict{Pass: true, Reason: "inline query guardrail not evaluated offline (m2)"}
	}
	value := slis[g.SLI]
	base := 0.0
	if baseline != nil {
		base = baseline[g.SLI]
	}
	pass, reason := EvalGuardrail(value, g, base)
	return GuardrailVerdict{SLI: g.SLI, Value: value, Pass: pass, Reason: reason}
}

// rankFeasible orders all feasible points best-first: Pareto-front members always
// rank ahead of dominated-but-feasible points, and within each tier the selection
// method (pareto-weighted or lexicographic) decides the order.
func rankFeasible(feasible []Point, frontSet map[string]bool, method string, weights map[string]float64, dirs map[string]string, obj v1alpha1.ObjectivesSpec) []string {
	pts := make([]Point, len(feasible))
	copy(pts, feasible)

	lexOrder := objectiveOrder(obj)
	better := func(a, b Point) bool {
		// Front tier wins outright.
		if frontSet[a.Hash] != frontSet[b.Hash] {
			return frontSet[a.Hash]
		}
		if method == MethodLexicographic {
			for _, name := range lexOrder {
				av, bv := a.Objectives[name], b.Objectives[name]
				if av != bv {
					return betterThan(av, bv, dirs[name])
				}
			}
			return a.Hash < b.Hash
		}
		sa, sb := WeightedScore(a, weights, dirs), WeightedScore(b, weights, dirs)
		if sa == sb {
			return a.Hash < b.Hash
		}
		return sa > sb // higher score is better
	}
	sort.SliceStable(pts, func(i, j int) bool { return better(pts[i], pts[j]) })

	out := make([]string, len(pts))
	for i, p := range pts {
		out[i] = p.Hash
	}
	return out
}

// objectivesOf extracts the direction map, the primary-heavy default weights, and
// the ordered objective SLI names from the study's objectives.
func objectivesOf(obj v1alpha1.ObjectivesSpec) (dirs map[string]string, weights map[string]float64, names []string) {
	dirs = map[string]string{}
	weights = map[string]float64{}
	if obj.Primary.SLI != "" {
		dirs[obj.Primary.SLI] = obj.Primary.Direction
		weights[obj.Primary.SLI] = 1.0 // primary-heavy default (§12)
		names = append(names, obj.Primary.SLI)
	}
	for _, s := range obj.Secondary {
		if s.SLI == "" || dirs[s.SLI] != "" {
			continue
		}
		dirs[s.SLI] = s.Direction
		weights[s.SLI] = 0.5
		names = append(names, s.SLI)
	}
	return dirs, weights, names
}

func objectiveOrder(obj v1alpha1.ObjectivesSpec) []string {
	order := []string{}
	if obj.Primary.SLI != "" {
		order = append(order, obj.Primary.SLI)
	}
	for _, s := range obj.Secondary {
		order = append(order, s.SLI)
	}
	return order
}

func methodOrDefault(m string) string {
	if m == MethodLexicographic {
		return MethodLexicographic
	}
	return MethodParetoWeighted
}

func topKOrDefault(k int32) int {
	if k <= 0 {
		return 3 // DESIGN.md §12 default
	}
	return int(k)
}

func sortedKeys(m map[string]aggregate) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

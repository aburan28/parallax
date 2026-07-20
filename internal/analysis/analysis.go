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

// Package analysis turns SLI facts into candidates: guardrail evaluation, the
// Pareto front, and weighted scoring (DESIGN.md §12). It is a pure function over
// results rows so selection is re-runnable offline and auditable.
package analysis

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/aburan28/parallax/api/v1alpha1"
)

// Direction values used in the dirs maps (matching the CRD Objective.Direction
// enum).
const (
	DirectionMinimize = "minimize"
	DirectionMaximize = "maximize"
)

// Point is one evaluated configuration: its objective and guardrail SLI values
// plus whether it cleared all guardrails.
type Point struct {
	// Hash is the canonical config hash (see internal/space.CanonicalHash).
	Hash string
	// Objectives maps objective SLI name -> value.
	Objectives map[string]float64
	// Guardrails maps guardrail SLI name -> value (for the record).
	Guardrails map[string]float64
	// Feasible is true when every guardrail passed.
	Feasible bool
}

// ParetoFront returns the non-dominated feasible points. dirs maps each
// objective name to "minimize" or "maximize" (unknown/empty is treated as
// minimize). Infeasible points never appear on the front: they inform the
// search but cannot be promoted (§12).
func ParetoFront(points []Point, dirs map[string]string) []Point {
	feasible := make([]Point, 0, len(points))
	for _, p := range points {
		if p.Feasible {
			feasible = append(feasible, p)
		}
	}

	front := make([]Point, 0, len(feasible))
	for i := range feasible {
		dominated := false
		for j := range feasible {
			if i == j {
				continue
			}
			if dominates(feasible[j], feasible[i], dirs) {
				dominated = true
				break
			}
		}
		if !dominated {
			front = append(front, feasible[i])
		}
	}
	return front
}

// dominates reports whether a dominates b: no worse on any objective and
// strictly better on at least one.
func dominates(a, b Point, dirs map[string]string) bool {
	better := false
	for name, dir := range dirs {
		av := a.Objectives[name]
		bv := b.Objectives[name]
		switch {
		case betterThan(av, bv, dir):
			better = true
		case betterThan(bv, av, dir):
			return false // b is strictly better here, so a cannot dominate
		}
	}
	return better
}

func betterThan(x, y float64, dir string) bool {
	if strings.EqualFold(dir, DirectionMaximize) {
		return x > y
	}
	return x < y // default: minimize
}

// WeightedScore is the normalized weighted sum used to rank the Pareto front
// (§12 pareto-weighted). Weights are normalized to sum to 1 (equal weights when
// all are zero) and each objective is oriented so that a higher score is always
// better (minimize objectives are negated).
//
// TODO(m1): apply per-objective min-max scaling across the candidate set before
// the weighted sum so objectives on different magnitudes are comparable; the
// single-Point signature here assumes the caller has already normalized values
// (or accepts raw-magnitude scoring for M0).
func WeightedScore(p Point, weights map[string]float64, dirs map[string]string) float64 {
	total := 0.0
	for _, w := range weights {
		total += math.Abs(w)
	}
	useEqual := total == 0
	n := len(weights)

	score := 0.0
	for name, w := range weights {
		var wn float64
		if useEqual {
			if n == 0 {
				continue
			}
			wn = 1.0 / float64(n)
		} else {
			wn = w / total
		}
		v := p.Objectives[name]
		if !strings.EqualFold(dirs[name], DirectionMaximize) {
			v = -v // orient so higher score is better
		}
		score += wn * v
	}
	return score
}

// EvalGuardrail evaluates one guardrail against a measured value, using baseline
// for relative limits. It parses the CRD DecimalString thresholds (plain numbers
// and trailing %). It returns whether the value passes and, on failure, a human
// reason recorded in the decision record. A malformed threshold fails closed
// with a descriptive reason rather than panicking.
func EvalGuardrail(value float64, g v1alpha1.Guardrail, baseline float64) (pass bool, reason string) {
	if g.Max != nil {
		num, pct, err := parseDecimal(*g.Max)
		if err != nil {
			return false, fmt.Sprintf("malformed max threshold %q: %v", string(*g.Max), err)
		}
		thr := num
		if pct {
			thr = num / 100
		}
		if value > thr {
			return false, fmt.Sprintf("value %g exceeds max %g", value, thr)
		}
	}

	if g.Min != nil {
		num, pct, err := parseDecimal(*g.Min)
		if err != nil {
			return false, fmt.Sprintf("malformed min threshold %q: %v", string(*g.Min), err)
		}
		thr := num
		if pct {
			thr = num / 100
		}
		if value < thr {
			return false, fmt.Sprintf("value %g below min %g", value, thr)
		}
	}

	if g.MaxRelativeToBaseline != nil {
		num, pct, err := parseDecimal(*g.MaxRelativeToBaseline)
		if err != nil {
			return false, fmt.Sprintf("malformed maxRelativeToBaseline %q: %v", string(*g.MaxRelativeToBaseline), err)
		}
		var limit float64
		if pct {
			// "5%" => baseline + 5%
			limit = baseline * (1 + num/100)
		} else {
			// "1.05" => baseline * 1.05
			limit = baseline * num
		}
		if value > limit {
			return false, fmt.Sprintf("value %g exceeds %g (baseline %g × %s)", value, limit, baseline, string(*g.MaxRelativeToBaseline))
		}
	}

	return true, ""
}

// parseDecimal parses a DecimalString, reporting whether it carried a trailing
// percent sign. The caller decides how a percent is interpreted.
func parseDecimal(d v1alpha1.DecimalString) (num float64, percent bool, err error) {
	s := strings.TrimSpace(string(d))
	if strings.HasSuffix(s, "%") {
		percent = true
		s = strings.TrimSpace(strings.TrimSuffix(s, "%"))
	}
	f, perr := strconv.ParseFloat(s, 64)
	if perr != nil {
		return 0, percent, fmt.Errorf("invalid decimal %q: %w", string(d), perr)
	}
	return f, percent, nil
}

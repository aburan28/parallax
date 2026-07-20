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

// Package stats is the core, non-pluggable statistics engine for validation
// (DESIGN.md §13.2). Promotion math must be identical everywhere for decisions
// to be comparable and auditable, so these functions are deliberately pure and
// dependency-free (standard library only).
package stats

import (
	"math"
	"sort"
)

// MannWhitneyU computes the Mann-Whitney U statistic for samples a and b and a
// two-sided p-value via the normal approximation with tie correction and a
// continuity correction. The returned u is min(U_a, U_b).
//
// One-sided callers (validation runs a one-sided test whose direction comes
// from the objective spec, §13.2) derive their p-value as p/2 when the observed
// effect is in the hypothesised direction, else 1 - p/2.
func MannWhitneyU(a, b []float64) (u float64, p float64) {
	n1 := len(a)
	n2 := len(b)
	if n1 == 0 || n2 == 0 {
		return 0, 1
	}

	// Rank the combined sample with average ranks for ties, accumulating the
	// rank sum of group a and the tie-correction term sum(t^3 - t).
	type item struct {
		v float64
		g int // 0 = a, 1 = b
	}
	all := make([]item, 0, n1+n2)
	for _, x := range a {
		all = append(all, item{x, 0})
	}
	for _, x := range b {
		all = append(all, item{x, 1})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].v < all[j].v })

	var r1 float64
	var tieSum float64
	for i := 0; i < len(all); {
		j := i
		for j < len(all) && all[j].v == all[i].v {
			j++
		}
		// Positions i..j-1 (0-based); 1-based ranks average to (i+1 + j)/2.
		avgRank := (float64(i+1) + float64(j)) / 2
		for k := i; k < j; k++ {
			if all[k].g == 0 {
				r1 += avgRank
			}
		}
		t := float64(j - i)
		tieSum += t*t*t - t
		i = j
	}

	fn1 := float64(n1)
	fn2 := float64(n2)
	n := fn1 + fn2

	u1 := r1 - fn1*(fn1+1)/2
	u2 := fn1*fn2 - u1
	u = math.Min(u1, u2)

	meanU := fn1 * fn2 / 2
	// Variance with tie correction.
	variance := (fn1 * fn2 / 12) * ((n + 1) - tieSum/(n*(n-1)))
	if variance <= 0 {
		// No spread (e.g. all values identical): no evidence of a difference.
		return u, 1
	}
	sigma := math.Sqrt(variance)

	// Continuity-corrected z on |U1 - meanU|.
	z := (math.Abs(u1-meanU) - 0.5) / sigma
	if z < 0 {
		z = 0
	}
	p = math.Erfc(z / math.Sqrt2) // == 2*(1 - Phi(z))
	if p > 1 {
		p = 1
	}
	if p < 0 {
		p = 0
	}
	return u, p
}

// CliffsDelta is the non-parametric effect size in [-1, 1]:
// (#(a>b) - #(a<b)) / (n1*n2). Positive means a tends to exceed b.
func CliffsDelta(a, b []float64) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	var gt, lt int
	for _, x := range a {
		for _, y := range b {
			switch {
			case x > y:
				gt++
			case x < y:
				lt++
			}
		}
	}
	return float64(gt-lt) / float64(len(a)*len(b))
}

// HolmCorrection applies the Holm-Bonferroni step-down procedure and returns
// adjusted p-values in the same order as the input (DESIGN.md §13.2).
func HolmCorrection(p []float64) []float64 {
	m := len(p)
	adj := make([]float64, m)
	if m == 0 {
		return adj
	}

	order := make([]int, m)
	for i := range order {
		order[i] = i
	}
	sort.Slice(order, func(i, j int) bool { return p[order[i]] < p[order[j]] })

	prev := 0.0
	for rank, idx := range order {
		a := float64(m-rank) * p[idx]
		if a < prev { // enforce monotone non-decreasing along the sorted order
			a = prev
		}
		if a > 1 {
			a = 1
		}
		adj[idx] = a
		prev = a
	}
	return adj
}

// TheilSen is the robust (median-of-slopes) linear fit used by the soak scenario
// to detect monotonic memory growth (DESIGN.md §13.3). It returns the slope and
// the intercept (median of y - slope*x). With fewer than two distinct x values
// it returns (0, median(ys)).
func TheilSen(xs, ys []float64) (slope, intercept float64) {
	n := len(xs)
	if n != len(ys) {
		// Defensive: operate on the common prefix rather than panicking.
		if len(ys) < n {
			n = len(ys)
		}
	}
	if n < 2 {
		return 0, median(ys)
	}

	slopes := make([]float64, 0, n*(n-1)/2)
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			dx := xs[j] - xs[i]
			if dx == 0 {
				continue
			}
			slopes = append(slopes, (ys[j]-ys[i])/dx)
		}
	}
	if len(slopes) == 0 {
		return 0, median(ys[:n])
	}
	slope = median(slopes)

	residuals := make([]float64, n)
	for i := 0; i < n; i++ {
		residuals[i] = ys[i] - slope*xs[i]
	}
	intercept = median(residuals)
	return slope, intercept
}

// median returns the median of vals without mutating the input.
func median(vals []float64) float64 {
	n := len(vals)
	if n == 0 {
		return 0
	}
	c := make([]float64, n)
	copy(c, vals)
	sort.Float64s(c)
	mid := n / 2
	if n%2 == 1 {
		return c[mid]
	}
	return (c[mid-1] + c[mid]) / 2
}

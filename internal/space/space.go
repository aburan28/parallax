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

// Package space resolves a Study's search space (DESIGN.md §8, §14) into a
// concrete, enumerable structure and provides the canonical assignment hashing
// used for trial dedupe/resume. It adopts the space abstractions from kapture
// draft PR #18 (Int/Float/Categorical, log scales) with CEL constraints layered
// on top.
package space

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strconv"
	"strings"

	"github.com/aburan28/parallax/api/v1alpha1"
)

// Kind discriminates the three dimension flavours.
type Kind int

const (
	// KindInt is an integer-valued axis (optionally log-scaled).
	KindInt Kind = iota
	// KindFloat is a real-valued axis (optionally log-scaled).
	KindFloat
	// KindCategorical is a finite set of string choices.
	KindCategorical
)

// Tuning knobs for M0 enumeration. These bound otherwise-explosive grids so a
// grid strategy over a wide linear range stays usable in the skeleton.
const (
	// maxLinearIntPoints caps how many values a single linear integer axis
	// contributes to a grid; wider ranges are sampled evenly. Log stepping is
	// naturally bounded and ignores this cap.
	maxLinearIntPoints = 64
	// gridFloatSteps is the fixed number of samples a float axis contributes to
	// a grid in M0. TODO(m1): make float grid density configurable per strategy.
	gridFloatSteps = 5
	// maxGridPoints guards the cartesian product size to avoid OOM on a
	// pathological space. GridPoints errors rather than materialising more.
	maxGridPoints = 1 << 20
)

// Dim is a resolved dimension: exactly one Kind with its parsed bounds/values.
type Dim struct {
	Name string
	Kind Kind

	// Integer bounds (KindInt).
	IntMin int64
	IntMax int64

	// Float bounds (KindFloat), parsed from the CRD DecimalString fields.
	FloatMin float64
	FloatMax float64

	// Log marks a log-scaled numeric axis (KindInt/KindFloat).
	Log bool

	// Values are the categorical choices (KindCategorical).
	Values []string
}

// Space is a resolved, enumerable search space plus its (not-yet-evaluated) CEL
// constraints.
type Space struct {
	Dims        []Dim
	Constraints []string
}

// Resolve builds a Space from a full SpaceSpec, carrying constraints through.
func Resolve(spec v1alpha1.SpaceSpec) (*Space, error) {
	s, err := New(spec.Dimensions)
	if err != nil {
		return nil, err
	}
	s.Constraints = append([]string(nil), spec.Constraints...)
	return s, nil
}

// New resolves a slice of CRD dimensions into a Space. Each Dimension must set
// exactly one of Int/Float/Categorical; anything else is a descriptive error.
func New(dims []v1alpha1.Dimension) (*Space, error) {
	if len(dims) == 0 {
		return nil, fmt.Errorf("space: at least one dimension is required")
	}
	out := &Space{Dims: make([]Dim, 0, len(dims))}
	for _, d := range dims {
		set := 0
		if d.Int != nil {
			set++
		}
		if d.Float != nil {
			set++
		}
		if d.Categorical != nil {
			set++
		}
		if set != 1 {
			return nil, fmt.Errorf("space: dimension %q must set exactly one of int/float/categorical (got %d)", d.Name, set)
		}
		if strings.TrimSpace(d.Name) == "" {
			return nil, fmt.Errorf("space: dimension has empty name")
		}

		rd := Dim{Name: d.Name}
		switch {
		case d.Int != nil:
			if d.Int.Min > d.Int.Max {
				return nil, fmt.Errorf("space: dimension %q int min %d > max %d", d.Name, d.Int.Min, d.Int.Max)
			}
			rd.Kind = KindInt
			rd.IntMin = d.Int.Min
			rd.IntMax = d.Int.Max
			rd.Log = d.Int.Log
		case d.Float != nil:
			lo, err := parseDecimal(d.Float.Min)
			if err != nil {
				return nil, fmt.Errorf("space: dimension %q float min: %w", d.Name, err)
			}
			hi, err := parseDecimal(d.Float.Max)
			if err != nil {
				return nil, fmt.Errorf("space: dimension %q float max: %w", d.Name, err)
			}
			if lo > hi {
				return nil, fmt.Errorf("space: dimension %q float min %g > max %g", d.Name, lo, hi)
			}
			rd.Kind = KindFloat
			rd.FloatMin = lo
			rd.FloatMax = hi
			rd.Log = d.Float.Log
		case d.Categorical != nil:
			if len(d.Categorical.Values) == 0 {
				return nil, fmt.Errorf("space: dimension %q categorical has no values", d.Name)
			}
			rd.Kind = KindCategorical
			rd.Values = append([]string(nil), d.Categorical.Values...)
		}
		out.Dims = append(out.Dims, rd)
	}
	return out, nil
}

// CanonicalHash returns the hex SHA-256 over the assignment map rendered as
// sorted "k=v;" pairs. It is the stable identity used for trial dedupe/resume
// (DESIGN.md §14, §15 config_hash).
func CanonicalHash(assignments map[string]string) string {
	keys := make([]string, 0, len(assignments))
	for k := range assignments {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(assignments[k])
		b.WriteByte(';')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// BaselineHash returns the deterministic config hash of a study's baseline point.
// The Study controller stamps this exact value on the baseline screening trial, and
// `parallax select` recomputes it to identify the baseline row — so both must use
// this one definition.
func BaselineHash(spec v1alpha1.StudySpec) string {
	var raw []byte
	if spec.Baseline.Values != nil {
		raw = spec.Baseline.Values.Raw
	}
	if len(raw) == 0 {
		raw = []byte(spec.Baseline.Name)
	}
	sum := sha256.Sum256(append([]byte("baseline:"), raw...))
	return hex.EncodeToString(sum[:])[:16]
}

// Satisfies reports whether an assignment passes the space's CEL constraints.
//
// TODO(m1): compile and evaluate the CEL Constraints (google/cel-go is already
// an indirect dep). M0 is an accept-all stub so wiring is exercised end to end.
func (s *Space) Satisfies(assignments map[string]string) (bool, error) {
	_ = assignments
	return true, nil
}

// GridPoints enumerates the cartesian product of every dimension. Integer and
// categorical axes are enumerated fully (log integers use ×2 geometric steps,
// wide linear ranges are evenly sub-sampled); float axes contribute a fixed
// number of grid samples. Points are filtered through the constraint stub.
func GridPoints(s *Space) ([]map[string]string, error) {
	if s == nil {
		return nil, fmt.Errorf("space: nil space")
	}
	names := make([]string, len(s.Dims))
	valueLists := make([][]string, len(s.Dims))
	for i, d := range s.Dims {
		names[i] = d.Name
		vs, err := gridValues(d)
		if err != nil {
			return nil, err
		}
		valueLists[i] = vs
	}

	points, err := cartesian(names, valueLists)
	if err != nil {
		return nil, err
	}

	filtered := points[:0]
	for _, p := range points {
		ok, err := s.Satisfies(p)
		if err != nil {
			return nil, fmt.Errorf("space: constraint evaluation: %w", err)
		}
		if ok {
			filtered = append(filtered, p)
		}
	}
	return filtered, nil
}

// RandomPoint draws one assignment uniformly (log-uniformly for log axes) using
// a deterministic source seeded by seed.
func RandomPoint(s *Space, seed int64) (map[string]string, error) {
	if s == nil {
		return nil, fmt.Errorf("space: nil space")
	}
	rng := rand.New(rand.NewSource(seed))
	out := make(map[string]string, len(s.Dims))
	for _, d := range s.Dims {
		switch d.Kind {
		case KindInt:
			out[d.Name] = strconv.FormatInt(randomInt(rng, d), 10)
		case KindFloat:
			out[d.Name] = formatFloat(randomFloat(rng, d))
		case KindCategorical:
			out[d.Name] = d.Values[rng.Intn(len(d.Values))]
		default:
			return nil, fmt.Errorf("space: dimension %q has unknown kind %d", d.Name, d.Kind)
		}
	}
	return out, nil
}

func randomInt(rng *rand.Rand, d Dim) int64 {
	if d.IntMin == d.IntMax {
		return d.IntMin
	}
	if d.Log && d.IntMin > 0 {
		lm := math.Log(float64(d.IntMin))
		lM := math.Log(float64(d.IntMax))
		v := math.Round(math.Exp(lm + rng.Float64()*(lM-lm)))
		return clampInt(int64(v), d.IntMin, d.IntMax)
	}
	span := d.IntMax - d.IntMin + 1
	return d.IntMin + rng.Int63n(span)
}

func randomFloat(rng *rand.Rand, d Dim) float64 {
	if d.FloatMin == d.FloatMax {
		return d.FloatMin
	}
	if d.Log && d.FloatMin > 0 {
		lm := math.Log(d.FloatMin)
		lM := math.Log(d.FloatMax)
		return math.Exp(lm + rng.Float64()*(lM-lm))
	}
	return d.FloatMin + rng.Float64()*(d.FloatMax-d.FloatMin)
}

func gridValues(d Dim) ([]string, error) {
	switch d.Kind {
	case KindInt:
		var vs []int64
		if d.Log {
			vs = intLogGrid(d.IntMin, d.IntMax)
		} else {
			vs = intLinGrid(d.IntMin, d.IntMax)
		}
		out := make([]string, len(vs))
		for i, v := range vs {
			out[i] = strconv.FormatInt(v, 10)
		}
		return out, nil
	case KindFloat:
		var vs []float64
		if d.Log {
			vs = floatLogGrid(d.FloatMin, d.FloatMax, gridFloatSteps)
		} else {
			vs = floatLinGrid(d.FloatMin, d.FloatMax, gridFloatSteps)
		}
		out := make([]string, len(vs))
		for i, v := range vs {
			out[i] = formatFloat(v)
		}
		return out, nil
	case KindCategorical:
		if len(d.Values) == 0 {
			return nil, fmt.Errorf("space: dimension %q has no categorical values", d.Name)
		}
		return append([]string(nil), d.Values...), nil
	default:
		return nil, fmt.Errorf("space: dimension %q has unknown kind %d", d.Name, d.Kind)
	}
}

// intLogGrid returns ×2 geometric steps from min to max inclusive. Falls back to
// linear when min is non-positive (log is undefined there).
func intLogGrid(min, max int64) []int64 {
	if min <= 0 {
		return intLinGrid(min, max)
	}
	set := map[int64]struct{}{min: {}, max: {}}
	for v := min; v <= max; v *= 2 {
		set[v] = struct{}{}
		if v > max/2 { // next doubling would exceed max (and could overflow)
			break
		}
	}
	return sortedInts(set)
}

// intLinGrid enumerates min..max inclusive, evenly sub-sampling when the range
// is wider than maxLinearIntPoints.
func intLinGrid(min, max int64) []int64 {
	if min > max {
		return nil
	}
	count := max - min + 1
	if count <= maxLinearIntPoints {
		out := make([]int64, 0, count)
		for v := min; v <= max; v++ {
			out = append(out, v)
		}
		return out
	}
	step := count / maxLinearIntPoints
	if count%maxLinearIntPoints != 0 {
		step++
	}
	out := make([]int64, 0, maxLinearIntPoints+1)
	for v := min; v <= max; v += step {
		out = append(out, v)
	}
	if out[len(out)-1] != max {
		out = append(out, max)
	}
	return out
}

func floatLinGrid(min, max float64, steps int) []float64 {
	if steps < 1 {
		steps = 1
	}
	if steps == 1 || min == max {
		return []float64{min}
	}
	out := make([]float64, steps)
	for i := 0; i < steps; i++ {
		out[i] = min + (max-min)*float64(i)/float64(steps-1)
	}
	return out
}

func floatLogGrid(min, max float64, steps int) []float64 {
	if min <= 0 { // log undefined; degrade to linear
		return floatLinGrid(min, max, steps)
	}
	if steps < 1 {
		steps = 1
	}
	if steps == 1 || min == max {
		return []float64{min}
	}
	lm := math.Log(min)
	lM := math.Log(max)
	out := make([]float64, steps)
	for i := 0; i < steps; i++ {
		out[i] = math.Exp(lm + (lM-lm)*float64(i)/float64(steps-1))
	}
	return out
}

// cartesian materialises the product of per-dimension value lists, guarding the
// total against maxGridPoints.
func cartesian(names []string, valueLists [][]string) ([]map[string]string, error) {
	total := 1
	for i, vs := range valueLists {
		if len(vs) == 0 {
			return nil, fmt.Errorf("space: dimension %q contributed no grid values", names[i])
		}
		total *= len(vs)
		if total > maxGridPoints {
			return nil, fmt.Errorf("space: grid product exceeds %d points; use a sampling strategy instead", maxGridPoints)
		}
	}

	out := make([]map[string]string, 0, total)
	idx := make([]int, len(valueLists))
	for {
		m := make(map[string]string, len(names))
		for d := range names {
			m[names[d]] = valueLists[d][idx[d]]
		}
		out = append(out, m)

		k := len(valueLists) - 1
		for k >= 0 {
			idx[k]++
			if idx[k] < len(valueLists[k]) {
				break
			}
			idx[k] = 0
			k--
		}
		if k < 0 {
			break
		}
	}
	return out, nil
}

// parseDecimal parses a CRD DecimalString (plain number or trailing %). A % is
// divided by 100 so "5%" -> 0.05.
func parseDecimal(d v1alpha1.DecimalString) (float64, error) {
	s := strings.TrimSpace(string(d))
	pct := false
	if strings.HasSuffix(s, "%") {
		pct = true
		s = strings.TrimSpace(strings.TrimSuffix(s, "%"))
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid decimal %q: %w", string(d), err)
	}
	if pct {
		f /= 100
	}
	return f, nil
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func clampInt(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func sortedInts(set map[int64]struct{}) []int64 {
	out := make([]int64, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

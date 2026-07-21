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

package space

import (
	"testing"

	"github.com/aburan28/parallax/api/v1alpha1"
)

func TestCanonicalHash_OrderIndependent(t *testing.T) {
	a := CanonicalHash(map[string]string{"x": "1", "y": "2"})
	b := CanonicalHash(map[string]string{"y": "2", "x": "1"})
	if a != b {
		t.Fatalf("hash must be order-independent: %s vs %s", a, b)
	}
	c := CanonicalHash(map[string]string{"x": "1", "y": "3"})
	if a == c {
		t.Fatalf("different assignments must hash differently")
	}
}

func TestBaselineHash_StableAndShort(t *testing.T) {
	spec := v1alpha1.StudySpec{
		Baseline: v1alpha1.BaselineSpec{Name: "shipped-defaults"},
	}
	h1 := BaselineHash(spec)
	h2 := BaselineHash(spec)
	if h1 != h2 {
		t.Fatalf("baseline hash must be stable: %s vs %s", h1, h2)
	}
	if len(h1) != 16 {
		t.Fatalf("baseline hash must be 16 hex chars, got %d (%q)", len(h1), h1)
	}
}

func TestGridPoints_CategoricalCartesian(t *testing.T) {
	sp, err := New([]v1alpha1.Dimension{
		{Name: "a", Categorical: &v1alpha1.CategoricalValues{Values: []string{"1", "2"}}},
		{Name: "b", Categorical: &v1alpha1.CategoricalValues{Values: []string{"x", "y", "z"}}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pts, err := GridPoints(sp)
	if err != nil {
		t.Fatalf("GridPoints: %v", err)
	}
	if len(pts) != 6 { // 2 × 3 cartesian product
		t.Fatalf("expected 6 grid points, got %d", len(pts))
	}
	// Every point must assign both dimensions, and hashes must be unique.
	seen := map[string]bool{}
	for _, p := range pts {
		if p["a"] == "" || p["b"] == "" {
			t.Fatalf("incomplete assignment: %v", p)
		}
		h := CanonicalHash(p)
		if seen[h] {
			t.Fatalf("duplicate grid point: %v", p)
		}
		seen[h] = true
	}
}

func TestGridPoints_IntRangeBounded(t *testing.T) {
	sp, err := New([]v1alpha1.Dimension{
		{Name: "n", Int: &v1alpha1.IntRange{Min: 1, Max: 3}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	pts, err := GridPoints(sp)
	if err != nil {
		t.Fatalf("GridPoints: %v", err)
	}
	if len(pts) == 0 {
		t.Fatalf("int range must yield at least one grid point")
	}
	for _, p := range pts {
		if p["n"] == "" {
			t.Fatalf("int dimension unassigned: %v", p)
		}
	}
}

func TestRandomPoint_DeterministicBySeed(t *testing.T) {
	sp, err := New([]v1alpha1.Dimension{
		{Name: "n", Int: &v1alpha1.IntRange{Min: 1, Max: 1000}},
		{Name: "c", Categorical: &v1alpha1.CategoricalValues{Values: []string{"a", "b", "c"}}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p1, err := RandomPoint(sp, 42)
	if err != nil {
		t.Fatalf("RandomPoint: %v", err)
	}
	p2, err := RandomPoint(sp, 42)
	if err != nil {
		t.Fatalf("RandomPoint: %v", err)
	}
	if CanonicalHash(p1) != CanonicalHash(p2) {
		t.Fatalf("same seed must give same point: %v vs %v", p1, p2)
	}
}

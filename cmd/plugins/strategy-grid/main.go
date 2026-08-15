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

// Command strategy-grid is the first-party "grid" strategy plugin: an exhaustive
// walk of the Cartesian product of every enumerated dimension.
//
// Ask is stateless. It rebuilds the grid from space_json on every call and skips
// whatever the host reports as already observed or in flight, so the plugin holds no
// cursor: it can be hot-reloaded mid-study, and one process can serve any number of
// concurrent studies without their searches colliding.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/aburan28/parallax/pkg/plugin"
	pluginv1 "github.com/aburan28/parallax/proto/plugin/v1"
)

// pluginName is the discovery/socket name (host looks for parallax-strategy-grid).
const pluginName = "grid"

// gridDim is one enumerated dimension of the search space. This is a deliberately
// tolerant subset of the DESIGN.md §8 space block: a dimension contributes to the
// grid only when it is named and carries a non-empty value list.
//
// Assignments are keyed by dimension **name**, never by path: `path` is where a
// target writes the value, which is the target plugin's business, not the search's.
type gridDim struct {
	Name        string   `json:"name"`
	Values      []string `json:"values"`
	Categorical *struct {
		Values []string `json:"values"`
	} `json:"categorical"`
}

// values returns the dimension's enumerated choices, accepting either the flat
// `values` form or the CRD's `categorical.values`.
func (d gridDim) values() []string {
	if len(d.Values) > 0 {
		return d.Values
	}
	if d.Categorical != nil {
		return d.Categorical.Values
	}
	return nil
}

// gridSpace is the parsed search-space document.
type gridSpace struct {
	Dimensions []gridDim `json:"dimensions"`
}

// server implements the Strategy service.
type server struct {
	pluginv1.UnimplementedStrategyServer
}

// Ask returns up to count grid points the host has not already taken, and reports
// done once the grid is exhausted.
func (s *server) Ask(_ context.Context, req *pluginv1.AskRequest) (*pluginv1.AskResponse, error) {
	var space gridSpace
	if raw := req.GetSpaceJson(); len(raw) > 0 {
		if err := json.Unmarshal(raw, &space); err != nil {
			return nil, fmt.Errorf("grid: parse space_json: %w", err)
		}
	}
	n := int(req.GetCount())
	if n <= 0 {
		n = 1
	}

	taken := map[string]bool{}
	for _, o := range req.GetObservations() {
		taken[o.GetConfigHash()] = true
	}
	for _, h := range req.GetPending() {
		taken[h] = true
	}

	points := buildGrid(space.Dimensions)
	out := make([]*pluginv1.Suggestion, 0, n)
	remaining := 0
	for _, p := range points {
		if taken[p.GetConfigHash()] {
			continue
		}
		remaining++
		if len(out) < n {
			out = append(out, p)
		}
	}

	return &pluginv1.AskResponse{
		Suggestions: out,
		Done:        remaining == 0,
		Detail:      fmt.Sprintf("grid: %d point(s) total, %d remaining", len(points), remaining),
	}, nil
}

// buildGrid returns the Cartesian product of every well-formed dimension as an
// ordered list of suggestions. Dimensions without a name or without values are
// skipped. If no dimension contributes, the grid is empty.
func buildGrid(dims []gridDim) []*pluginv1.Suggestion {
	combos := []map[string]string{{}}
	for _, d := range dims {
		vals := d.values()
		if d.Name == "" || len(vals) == 0 {
			continue
		}
		next := make([]map[string]string, 0, len(combos)*len(vals))
		for _, base := range combos {
			for _, v := range vals {
				m := make(map[string]string, len(base)+1)
				for k, vv := range base {
					m[k] = vv
				}
				m[d.Name] = v
				next = append(next, m)
			}
		}
		combos = next
	}
	// No dimension contributed: avoid emitting a single empty point.
	if len(combos) == 1 && len(combos[0]) == 0 {
		return nil
	}
	out := make([]*pluginv1.Suggestion, 0, len(combos))
	for _, m := range combos {
		out = append(out, &pluginv1.Suggestion{
			ConfigHash:  plugin.CanonicalHash(m),
			Assignments: m,
		})
	}
	// Deterministic order: the same space always walks in the same sequence.
	sort.Slice(out, func(i, j int) bool { return out[i].ConfigHash < out[j].ConfigHash })
	return out
}

func main() {
	impl := &server{}
	cfg := plugin.ServeConfig{
		Name: pluginName,
		Kind: pluginv1.PluginKind_PLUGIN_KIND_STRATEGY,
		Lifecycle: &plugin.BaseLifecycle{
			Name: pluginName,
			Kind: pluginv1.PluginKind_PLUGIN_KIND_STRATEGY,
		},
		Strategy: impl,
	}
	if err := plugin.Serve(cfg); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

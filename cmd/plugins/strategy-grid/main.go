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

// Command strategy-grid is the first-party "grid" strategy plugin. It performs an
// exhaustive grid search: Init parses the search space into the Cartesian product
// of each dimension's enumerated values, and successive Ask calls hand back the
// next batch of grid points.
//
// M0: Init and Ask are real; Tell and Report remain unimplemented via the
// embedded UnimplementedStrategyServer (grid search ignores feedback).
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"

	"github.com/aburan28/parallax/pkg/plugin"
	pluginv1 "github.com/aburan28/parallax/proto/plugin/v1"
)

// pluginName is the discovery/socket name (host looks for parallax-strategy-grid).
const pluginName = "grid"

// gridDim is one enumerated dimension of the search space. This is a deliberately
// tolerant subset of the DESIGN.md §8 space block: a dimension contributes to the
// grid only when it has a key (path, else name) and a non-empty value list.
type gridDim struct {
	Name   string   `json:"name"`
	Path   string   `json:"path"`
	Values []string `json:"values"`
}

// gridSpace is the parsed search-space document.
type gridSpace struct {
	Dimensions []gridDim `json:"dimensions"`
}

// server implements the Strategy service. Init/Ask are real; Tell/Report are
// left unimplemented for M0 via the embedded UnimplementedStrategyServer.
type server struct {
	pluginv1.UnimplementedStrategyServer

	mu     sync.Mutex
	points []*pluginv1.Suggestion // full grid, computed at Init
	cursor int                    // index of the next unserved point
}

// Init parses the search space and materializes the full grid.
func (s *server) Init(_ context.Context, req *pluginv1.InitRequest) (*pluginv1.InitResponse, error) {
	var space gridSpace
	raw := req.GetSpaceJson()
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &space); err != nil {
			return &pluginv1.InitResponse{Ok: false, Detail: fmt.Sprintf("grid: parse space_json: %v", err)}, nil
		}
	}
	points := buildGrid(space.Dimensions)

	s.mu.Lock()
	s.points = points
	s.cursor = 0
	s.mu.Unlock()

	return &pluginv1.InitResponse{Ok: true, Detail: fmt.Sprintf("grid: %d point(s)", len(points))}, nil
}

// Ask returns up to count of the not-yet-served grid points, advancing the cursor.
// When the grid is exhausted it returns an empty suggestion list.
func (s *server) Ask(_ context.Context, req *pluginv1.AskRequest) (*pluginv1.AskResponse, error) {
	n := int(req.GetCount())
	if n <= 0 {
		n = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]*pluginv1.Suggestion, 0, n)
	for i := 0; i < n && s.cursor < len(s.points); i++ {
		out = append(out, s.points[s.cursor])
		s.cursor++
	}
	return &pluginv1.AskResponse{Suggestions: out}, nil
}

// buildGrid returns the Cartesian product of every well-formed dimension as an
// ordered list of suggestions. Dimensions without a key or without values are
// skipped. If no dimension contributes, the grid is empty.
func buildGrid(dims []gridDim) []*pluginv1.Suggestion {
	combos := []map[string]string{{}}
	for _, d := range dims {
		key := d.Path
		if key == "" {
			key = d.Name
		}
		if key == "" || len(d.Values) == 0 {
			continue
		}
		next := make([]map[string]string, 0, len(combos)*len(d.Values))
		for _, base := range combos {
			for _, v := range d.Values {
				m := make(map[string]string, len(base)+1)
				for k, vv := range base {
					m[k] = vv
				}
				m[key] = v
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
			ConfigHash:  configHash(m),
			Assignments: m,
		})
	}
	return out
}

// configHash is a stable content hash of an assignment map (order-independent).
func configHash(assignments map[string]string) string {
	keys := make([]string, 0, len(assignments))
	for k := range assignments {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		_, _ = io.WriteString(h, k)
		_, _ = io.WriteString(h, "=")
		_, _ = io.WriteString(h, assignments[k])
		_, _ = io.WriteString(h, ";")
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
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

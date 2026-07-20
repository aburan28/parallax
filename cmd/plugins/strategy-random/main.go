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

// Command strategy-random is the first-party "random" strategy plugin. It performs
// uniform random search: Init parses the search space and seeds the RNG, and each
// Ask draws a uniformly random value from every enumerated dimension.
//
// M0: Init and Ask are real; Tell and Report remain unimplemented via the
// embedded UnimplementedStrategyServer (random search ignores feedback).
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/aburan28/parallax/pkg/plugin"
	pluginv1 "github.com/aburan28/parallax/proto/plugin/v1"
)

// pluginName is the discovery/socket name (host looks for parallax-strategy-random).
const pluginName = "random"

// randomDim is one enumerated dimension of the search space. This is a tolerant
// subset of the DESIGN.md §8 space block: a dimension is usable only when it has a
// key (path, else name) and a non-empty value list.
type randomDim struct {
	Name   string   `json:"name"`
	Path   string   `json:"path"`
	Values []string `json:"values"`
}

// randomSpace is the parsed search-space document.
type randomSpace struct {
	Dimensions []randomDim `json:"dimensions"`
}

// server implements the Strategy service. Init/Ask are real; Tell/Report are
// left unimplemented for M0 via the embedded UnimplementedStrategyServer.
type server struct {
	pluginv1.UnimplementedStrategyServer

	mu   sync.Mutex
	dims []randomDim
	rng  *rand.Rand
}

// Init parses the search space and seeds the RNG (falling back to a time seed
// when the host supplies seed 0).
func (s *server) Init(_ context.Context, req *pluginv1.InitRequest) (*pluginv1.InitResponse, error) {
	var space randomSpace
	raw := req.GetSpaceJson()
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &space); err != nil {
			return &pluginv1.InitResponse{Ok: false, Detail: fmt.Sprintf("random: parse space_json: %v", err)}, nil
		}
	}
	seed := req.GetSeed()
	if seed == 0 {
		seed = time.Now().UnixNano()
	}

	// Keep only usable dimensions so Ask stays branch-free.
	usable := make([]randomDim, 0, len(space.Dimensions))
	for _, d := range space.Dimensions {
		if d.Path == "" && d.Name == "" {
			continue
		}
		if len(d.Values) == 0 {
			continue
		}
		usable = append(usable, d)
	}

	s.mu.Lock()
	s.dims = usable
	s.rng = rand.New(rand.NewSource(seed))
	s.mu.Unlock()

	return &pluginv1.InitResponse{Ok: true, Detail: fmt.Sprintf("random: %d dimension(s), seed %d", len(usable), seed)}, nil
}

// Ask draws count random suggestions, one uniform pick per dimension.
func (s *server) Ask(_ context.Context, req *pluginv1.AskRequest) (*pluginv1.AskResponse, error) {
	n := int(req.GetCount())
	if n <= 0 {
		n = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.rng == nil { // Ask before Init: use a deterministic default RNG.
		s.rng = rand.New(rand.NewSource(1))
	}

	out := make([]*pluginv1.Suggestion, 0, n)
	for i := 0; i < n; i++ {
		assignments := make(map[string]string, len(s.dims))
		for _, d := range s.dims {
			key := d.Path
			if key == "" {
				key = d.Name
			}
			assignments[key] = d.Values[s.rng.Intn(len(d.Values))]
		}
		out = append(out, &pluginv1.Suggestion{
			ConfigHash:  configHash(assignments),
			Assignments: assignments,
		})
	}
	return &pluginv1.AskResponse{Suggestions: out}, nil
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

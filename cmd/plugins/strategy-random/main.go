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

// Command strategy-random is the first-party "random" strategy plugin: uniform
// random search, the permanent sanity floor every other strategy is measured
// against (DESIGN.md §14).
//
// Ask is stateless and deterministic. The RNG is seeded from (run seed, number of
// points already taken) rather than carried across calls, so a hot-reloaded plugin
// resumes the identical sequence and one process serves concurrent studies without
// their draws interfering.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"sort"
	"strconv"

	"github.com/aburan28/parallax/pkg/plugin"
	pluginv1 "github.com/aburan28/parallax/proto/plugin/v1"
)

// pluginName is the discovery/socket name (host looks for parallax-strategy-random).
const pluginName = "random"

// drawAttemptsPerPoint bounds rejection sampling when the space is nearly exhausted:
// without it, a small space with most points taken would spin.
const drawAttemptsPerPoint = 16

// randomDim is one dimension of the search space. Only the flavours a random draw
// can sample are modeled; assignments are keyed by dimension **name** (a `path` is
// the target's business, not the search's).
type randomDim struct {
	Name        string `json:"name"`
	Categorical *struct {
		Values []string `json:"values"`
	} `json:"categorical"`
	Int *struct {
		Min int64 `json:"min"`
		Max int64 `json:"max"`
	} `json:"int"`
	Float *struct {
		Min string `json:"min"`
		Max string `json:"max"`
	} `json:"float"`
}

// draw picks one value for this dimension, or "" (skip) when the dimension carries
// nothing samplable.
func (d randomDim) draw(rng *rand.Rand) (string, bool) {
	switch {
	case d.Categorical != nil && len(d.Categorical.Values) > 0:
		return d.Categorical.Values[rng.Intn(len(d.Categorical.Values))], true
	case d.Int != nil && d.Int.Max >= d.Int.Min:
		span := d.Int.Max - d.Int.Min + 1
		return strconv.FormatInt(d.Int.Min+rng.Int63n(span), 10), true
	case d.Float != nil:
		lo, errLo := strconv.ParseFloat(d.Float.Min, 64)
		hi, errHi := strconv.ParseFloat(d.Float.Max, 64)
		if errLo != nil || errHi != nil || hi < lo {
			return "", false
		}
		return strconv.FormatFloat(lo+rng.Float64()*(hi-lo), 'g', -1, 64), true
	default:
		return "", false
	}
}

type randomSpace struct {
	Dimensions []randomDim `json:"dimensions"`
}

// askConfig is this strategy's config block from the study's space.strategy.config.
type askConfig struct {
	// Points caps how many distinct config points the search covers. It counts every
	// point the host reports as taken — the study's baseline included — so it lines
	// up with budgets.maxTrials rather than competing with it. 0 = unbounded.
	Points int `json:"points"`
}

// server implements the Strategy service.
type server struct {
	pluginv1.UnimplementedStrategyServer
}

// Ask draws up to count fresh points, skipping any the host reports as taken.
func (s *server) Ask(_ context.Context, req *pluginv1.AskRequest) (*pluginv1.AskResponse, error) {
	var space randomSpace
	if raw := req.GetSpaceJson(); len(raw) > 0 {
		if err := json.Unmarshal(raw, &space); err != nil {
			return nil, fmt.Errorf("random: parse space_json: %w", err)
		}
	}
	var cfg askConfig
	if raw := req.GetConfigJson(); len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("random: parse config_json: %w", err)
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

	// The point budget is counted against what already exists, so a resumed study
	// does not restart the count.
	if cfg.Points > 0 && len(taken) >= cfg.Points {
		return &pluginv1.AskResponse{
			Done:   true,
			Detail: fmt.Sprintf("random: point budget %d reached", cfg.Points),
		}, nil
	}
	if cfg.Points > 0 && len(taken)+n > cfg.Points {
		n = cfg.Points - len(taken)
	}

	// Seeding from (run seed, points taken) makes the draw a pure function of the
	// history: replaying a run reproduces the same sequence.
	rng := rand.New(rand.NewSource(req.GetSeed() + int64(len(taken))))
	dims := usableDims(space.Dimensions)

	out := make([]*pluginv1.Suggestion, 0, n)
	drawn := map[string]bool{}
	for i := 0; i < n*drawAttemptsPerPoint && len(out) < n; i++ {
		assignments := map[string]string{}
		for _, d := range dims {
			if v, ok := d.draw(rng); ok {
				assignments[d.Name] = v
			}
		}
		if len(assignments) == 0 {
			break // nothing samplable in this space
		}
		h := plugin.CanonicalHash(assignments)
		if taken[h] || drawn[h] {
			continue // collision: try again
		}
		drawn[h] = true
		out = append(out, &pluginv1.Suggestion{ConfigHash: h, Assignments: assignments})
	}

	// An empty draw means the space is effectively exhausted — every attempt collided
	// with a point already taken.
	return &pluginv1.AskResponse{
		Suggestions: out,
		Done:        len(out) == 0,
		Detail:      fmt.Sprintf("random: drew %d point(s) over %d dimension(s)", len(out), len(dims)),
	}, nil
}

// usableDims keeps only named, samplable dimensions, in a stable order so the draw
// sequence does not depend on map iteration or document ordering quirks.
func usableDims(dims []randomDim) []randomDim {
	out := make([]randomDim, 0, len(dims))
	for _, d := range dims {
		if d.Name == "" {
			continue
		}
		if d.Categorical == nil && d.Int == nil && d.Float == nil {
			continue
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
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

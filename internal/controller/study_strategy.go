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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"google.golang.org/grpc"

	v1alpha1 "github.com/aburan28/parallax/api/v1alpha1"
	"github.com/aburan28/parallax/internal/pluginhost"
	"github.com/aburan28/parallax/internal/space"
	"github.com/aburan28/parallax/internal/store"
	pluginv1 "github.com/aburan28/parallax/proto/plugin/v1"
)

// builtinPrefix marks a compiled-in strategy. Built-ins exist so `--local` runs need
// no plugin install (DESIGN.md §14); naming them explicitly is what keeps the choice
// honest. Anything without this prefix is a plugin name, and an unresolvable one
// fails the study rather than quietly becoming a grid (docs/GENERALIZATION.md G3).
const builtinPrefix = "builtin:"

// Compiled-in strategies. These are the two the operator can run with no plugin
// installed; every other strategy — sobol, ASHA, Optuna, anything third-party — is a
// Plugin CR on the same ABI.
const (
	BuiltinGrid   = builtinPrefix + "grid"
	BuiltinRandom = builtinPrefix + "random"
)

// defaultAskBatch bounds how many points one reconcile materializes. Batching is what
// makes an adaptive strategy adaptive: it sees the previous batch's observations
// before suggesting the next. Grid and random ignore the feedback and just refill.
const defaultAskBatch = 8

// strategyConfig is the config block carried by a study's space.strategy. Only
// batchSize/seed are core semantics; the whole block is also passed through to the
// plugin verbatim, so a strategy's own knobs live here too.
type strategyConfig struct {
	// BatchSize bounds how many points are in flight before the strategy is asked
	// again. Defaults to defaultAskBatch.
	BatchSize int `json:"batchSize"`
	// Points caps how many distinct config points the search draws. Like
	// budgets.maxTrials, it counts *every* point including the baseline.
	Points int `json:"points"`
	// Seed makes a run reproducible; 0 derives one from the study UID.
	Seed int64 `json:"seed"`
}

// strategyAsker is the strategy call surface, so the sweep loop is unit-testable
// without launching subprocesses.
type strategyAsker interface {
	Ask(ctx context.Context, plugin string, req *pluginv1.AskRequest) (*pluginv1.AskResponse, error)
}

// hostStrategy dials a strategy plugin through the plugin host.
type hostStrategy struct {
	host pluginhost.Host
}

func (h hostStrategy) Ask(ctx context.Context, name string, req *pluginv1.AskRequest) (*pluginv1.AskResponse, error) {
	if h.host == nil {
		return nil, fmt.Errorf("no plugin host configured")
	}
	conn, err := h.host.Conn(name)
	if err != nil {
		return nil, fmt.Errorf("strategy plugin %q: %w", name, err)
	}
	var cc grpc.ClientConnInterface = conn
	return pluginv1.NewStrategyClient(cc).Ask(ctx, req)
}

// strategy returns the strategy call surface, defaulting to a host-backed adapter.
func (r *StudyReconciler) strategy() strategyAsker {
	if r.Strategy != nil {
		return r.Strategy
	}
	return hostStrategy{host: r.Host}
}

// askStrategy produces the next batch of config points for the sweep.
//
// The name resolves one of two ways, with no fuzzy matching and no silent fallback:
// a `builtin:` name runs compiled-in, anything else must be a Ready strategy plugin.
// An unknown builtin or an unavailable plugin is an error the caller turns into a
// failed study, because a study that silently searched a different space than the one
// it declared has produced worthless results.
func (r *StudyReconciler) askStrategy(ctx context.Context, study *v1alpha1.Study, in askInput) ([]map[string]string, bool, error) {
	name := strings.TrimSpace(study.Spec.Space.Strategy.Plugin)
	if name == "" {
		return nil, false, fmt.Errorf("space.strategy.plugin is empty; set a %s… builtin or a strategy plugin name", builtinPrefix)
	}
	sp, err := space.Resolve(study.Spec.Space)
	if err != nil {
		return nil, false, fmt.Errorf("resolve search space: %w", err)
	}

	if strings.HasPrefix(name, builtinPrefix) {
		return builtinAsk(name, sp, in)
	}
	return r.pluginAsk(ctx, name, study, in)
}

// askInput is what a strategy needs beyond the space: the budget, the seed, how many
// points are wanted, and everything observed or in flight so far.
type askInput struct {
	seed     int64
	count    int
	cfg      strategyConfig
	rawCfg   []byte
	budget   v1alpha1.BudgetSpec
	observed []*pluginv1.Observation
	pending  []string
	// seen is every config hash already materialized, so builtins can skip them
	// without re-deriving the set.
	seen map[string]bool
}

// builtinAsk runs a compiled-in strategy.
func builtinAsk(name string, sp *space.Space, in askInput) ([]map[string]string, bool, error) {
	switch name {
	case BuiltinGrid:
		// Grid is fully enumerable, so "done" is knowable: once every point has been
		// materialized there is nothing left to suggest.
		all, err := space.GridPoints(sp)
		if err != nil {
			return nil, false, err
		}
		out := make([]map[string]string, 0, in.count)
		for _, p := range all {
			if len(out) >= in.count {
				return out, false, nil
			}
			if !in.seen[space.CanonicalHash(p)] {
				out = append(out, p)
			}
		}
		return out, true, nil // walked the whole grid: exhausted

	case BuiltinRandom:
		// Random draws fresh points, skipping collisions. It is done when the study's
		// point budget is met, or when the space is too small to yield new draws.
		limit := in.cfg.Points
		if limit > 0 && len(in.seen) >= limit {
			return nil, true, nil
		}
		out := make([]map[string]string, 0, in.count)
		drawn := map[string]bool{}
		// Draw deterministically from (seed, how many points already exist), so a
		// replayed run reproduces the same sequence.
		for i := 0; i < in.count*16 && len(out) < in.count; i++ {
			p, err := space.RandomPoint(sp, in.seed+int64(len(in.seen)+i))
			if err != nil {
				return nil, false, err
			}
			h := space.CanonicalHash(p)
			if in.seen[h] || drawn[h] {
				continue
			}
			drawn[h] = true
			out = append(out, p)
			if limit > 0 && len(in.seen)+len(out) >= limit {
				return out, true, nil
			}
		}
		return out, len(out) == 0, nil

	default:
		return nil, false, fmt.Errorf("unknown builtin strategy %q (have %s, %s)", name, BuiltinGrid, BuiltinRandom)
	}
}

// pluginAsk calls a strategy plugin over the ABI.
func (r *StudyReconciler) pluginAsk(ctx context.Context, name string, study *v1alpha1.Study, in askInput) ([]map[string]string, bool, error) {
	spaceJSON, err := json.Marshal(study.Spec.Space)
	if err != nil {
		return nil, false, fmt.Errorf("marshal search space: %w", err)
	}
	budgetJSON, err := json.Marshal(in.budget)
	if err != nil {
		return nil, false, fmt.Errorf("marshal budget: %w", err)
	}

	resp, err := r.strategy().Ask(ctx, name, &pluginv1.AskRequest{
		SpaceJson:    spaceJSON,
		BudgetJson:   budgetJSON,
		Seed:         in.seed,
		Count:        int32(in.count),
		Observations: in.observed,
		Pending:      in.pending,
		ConfigJson:   in.rawCfg,
	})
	if err != nil {
		return nil, false, fmt.Errorf("strategy %q Ask: %w", name, err)
	}

	out := make([]map[string]string, 0, len(resp.GetSuggestions()))
	for _, s := range resp.GetSuggestions() {
		a := s.GetAssignments()
		if len(a) == 0 {
			continue
		}
		// The strategy's own hash is advisory: the controller re-derives the canonical
		// one so dedupe and resume never depend on a plugin hashing consistently.
		if in.seen[space.CanonicalHash(a)] {
			continue
		}
		out = append(out, a)
	}
	return out, resp.GetDone(), nil
}

// observations rebuilds the run's evaluation history from the results DB, which is
// the system of record — so a restarted operator, a hot-reloaded plugin, and a
// resumed study all see exactly the same history (§15, G3).
//
// The objective is normalized to "lower is better" here, so no strategy has to read
// the study's direction. Points whose objective SLI was never collected are skipped:
// a strategy must not be told a failed trial scored zero.
func (r *StudyReconciler) observations(ctx context.Context, study *v1alpha1.Study, runID int64) ([]*pluginv1.Observation, error) {
	// No run yet (or no store wired): the history is empty, which is the honest
	// answer for the first batch of a study.
	if r.Store == nil || runID == 0 {
		return nil, nil
	}
	trials, err := r.Store.ListTrialsForRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list trials for run %d: %w", runID, err)
	}
	if len(trials) == 0 {
		return nil, nil
	}
	values, err := r.Store.ListSLIValuesForRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("list sli values for run %d: %w", runID, err)
	}
	byTrial := map[int64]map[string]float64{}
	for _, v := range values {
		m := byTrial[v.TrialID]
		if m == nil {
			m = map[string]float64{}
			byTrial[v.TrialID] = m
		}
		m[v.Name] = v.Value
	}

	objective := study.Spec.Objectives.Primary.SLI
	maximize := study.Spec.Objectives.Primary.Direction == "maximize"

	out := make([]*pluginv1.Observation, 0, len(trials))
	for i := range trials {
		t := &trials[i]
		slis := byTrial[t.ID]
		value, ok := slis[objective]
		if !ok {
			continue // objective never collected: not an observation
		}
		if maximize {
			value = -value // strategies always minimize
		}
		metrics, err := json.Marshal(slis)
		if err != nil {
			return nil, fmt.Errorf("marshal trial %d metrics: %w", t.ID, err)
		}
		out = append(out, &pluginv1.Observation{
			ConfigHash:      t.ConfigHash,
			Assignments:     assignmentsFromTrial(t),
			Objective:       value,
			Feasible:        t.Validity != "invalid",
			FidelitySeconds: fidelitySeconds(t),
			Rep:             int32(t.Rep),
			MetricsJson:     metrics,
		})
	}
	// Stable ordering keeps Ask deterministic for a given history.
	sort.Slice(out, func(i, j int) bool {
		if out[i].ConfigHash != out[j].ConfigHash {
			return out[i].ConfigHash < out[j].ConfigHash
		}
		return out[i].Rep < out[j].Rep
	})
	return out, nil
}

// assignmentsFromTrial recovers a trial's dimension assignments from its persisted
// config blob. Best effort: a trial whose config did not round-trip contributes its
// objective without its coordinates.
func assignmentsFromTrial(t *store.TrialRecord) map[string]string {
	if len(t.Config) == 0 {
		return nil
	}
	var a map[string]string
	if err := json.Unmarshal(t.Config, &a); err != nil {
		return nil
	}
	return a
}

// fidelitySeconds reads the measurement window a trial was evaluated at, for
// multi-fidelity strategies. Zero when the record carries no fidelity block.
func fidelitySeconds(t *store.TrialRecord) float64 {
	if len(t.Fidelity) == 0 {
		return 0
	}
	var f struct {
		Measure string `json:"measure"`
	}
	if err := json.Unmarshal(t.Fidelity, &f); err != nil || f.Measure == "" {
		return 0
	}
	d, err := time.ParseDuration(f.Measure)
	if err != nil {
		return 0
	}
	return d.Seconds()
}

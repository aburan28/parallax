# Strategy conformance suite (the PR #18 synthetic problems)

This suite is the new home of kapture draft **PR #18**'s `hpo-benchmark/` harness
(DESIGN.md §19). The search-layer abstractions move into `internal/space`,
`internal/stats`, and `internal/analysis`; the harness's *synthetic problems* live
on here as the objective bar for strategy plugins. PR #18 can then close in kapture
with a pointer to this directory — kapture stays focused on capture/replay,
parallax owns experimentation.

## Why synthetic problems

A strategy (`grid`, `random`, `sobol`, `asha`, or a third-party `optuna`/SMAC/Ax
plugin) must be trustworthy *before* it is handed cluster hours. These problems
have known optima and run in **milliseconds**, so a plugin's `ask`/`tell`/`report`
loop is validated against ground truth without touching a cluster (§14).

## The problems

Ported from PR #18 `hpo-benchmark/problems/`:

| Problem | Shape | What it stresses |
|---|---|---|
| `NoisyBranin` | 2-D continuous, additive noise **and** low-fidelity bias | that a strategy tolerates noise and does not trust biased low-fidelity values |
| `Hartmann6` | 6-D continuous, multi-modal | exploration vs. exploitation in higher dimensions |
| `Ackley` | N-D, many local minima | escaping local optima |
| conditional / mixed space | `Categorical` parent gating child `Float`/`Int` (log scales) | parents-first ordering, constraint handling |

Low-fidelity evaluations are deliberately **biased, not merely noisy** — short
windows overweight warmup — mirroring why parallax only ever promotes on
validation-fidelity data (§14).

## What the suite asserts

- **ABI conformance** (inherited from the parent suite): handshake, Describe,
  Configure, Drain.
- **Ask/tell protocol**: `ask()` yields in-space configs; `tell(trial, obs)` is
  honored; `report(trial, step, value)` returns a prune decision for multi-fidelity.
- **Budget honesty**: the strategy never exceeds `Budget{maxTrials,maxCost,maxWallClock}`;
  optimizer thinking-time is accounted separately (a strategy that thinks for
  minutes must show it) — same cumulative-counter row shape as `observations` (§15).
- **Effectiveness floor**: best-so-far / simple-regret curves beat `strategy-random`
  (the permanent sanity floor) via median + IQR across seeds and mean-rank
  aggregation (PR #18 `analysis.py`), within a tolerance.

## Running

```sh
make conformance                                   # all first-party strategies
parallax plugin conformance ./bin/parallax-strategy-random
```

## Status

M0 scaffold: contract only. The Go harness + problem implementations land in M1
with `internal/space`, `internal/stats`, `internal/analysis` (agent F territory)
and the `strategy-{grid,random,sobol}` plugins.

<!-- TODO(m1): port PR #18 problems (NoisyBranin, Hartmann6, Ackley) to Go fixtures -->
<!-- TODO(m1): implement ask/tell/report + budget + regret-curve assertions -->

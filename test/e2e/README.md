# End-to-end tests (kind)

The e2e suite proves the closed loop on a real cluster: build images, load them
into **kind**, install the chart, and run a Study to completion — mirroring
kapture's e2e recipe (Appendix A.5). It is driven by
[`.github/workflows/e2e.yaml`](../../.github/workflows/e2e.yaml) and, locally, by
[`hack/env-up.sh`](../../hack/env-up.sh).

## The M0 target

> A 1-point `Study` runs end-to-end on kind (`--local`): SLI rows land in the DB,
> a report renders. (DESIGN.md §21, M0 exit criterion.)

That closed loop generalizes kapture's `loadtest_test.go` (120 seeded requests →
shard fan-out → per-cell rollup → `capture_agent_requests_total == 120`): parallax
adds the funnel — evaluate a config, collect SLIs over the measurement window,
write the decision to the store.

## Local run

```sh
hack/env-up.sh                 # kind + Gateway API + Envoy Gateway + MinIO + Prometheus (5s)
export KUBECONFIG=hack/kind-kubeconfig
make build
# --local runs the same controllers against kind with a SQLite store:
go run ./cmd/parallax apply --local examples/studies/capture-agent-throughput.yaml
hack/env-down.sh
```

## CI run

`e2e.yaml` builds `:e2e` images, `kind load`s them, stands the env up via
`hack/env-up.sh`, `helm upgrade --install --wait`s the chart (with
`db.localPostgres.enabled=true`), then runs the suite. The suite is gated by an
`E2E_ENABLED` flag (kapture uses `E2E_CAPTUREHUB_NAME`) so the pipeline is green
while the Go suite is still a skeleton.

## What lands here (M1+)

- `study_local_test.go` — a 1-point Study to `Collected`, asserting SLI rows +
  a rendered report from the SQLite store.
- `hotreload_test.go` — swap a plugin image, assert drain-and-swap keeps an
  in-flight trial intact (§5.4).
- `sweep_test.go` — a small grid study fans out Trials across the env.

## Status

M0 scaffold: contract only. The env-up path is real today; the Go suite is filled
in during M1.

<!-- TODO(m1): study_local_test.go (1-point Study end-to-end, SLI rows + report) -->
<!-- TODO(m2): hotreload_test.go, sweep_test.go -->

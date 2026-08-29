# parallax

**Enterprise-grade, open-source benchmarking and experimentation for Kubernetes — powered by [kapture](https://github.com/aburan28/kapture), extensible to the core through hot-reloadable gRPC plugins.**

Parallax answers one question rigorously: *of all the ways you could configure a system, which one should you actually run?*

It does this with a three-stage funnel:

1. **Sweep** — expand a declarative config matrix (or drive a search strategy) over the system under test, run short screening trials against each configuration under realistic load, and collect metrics for every trial.
2. **Select** — evaluate SLIs over each trial's measurement window through pluggable metrics providers (Prometheus first-class; OTel and others via plugins), enforce guardrails, and rank survivors on your objectives into a small set of *candidates*. Every selection is a recorded, reproducible decision stored in a durable results database.
3. **Validate** — promote nothing on screening data alone. Candidates face rigorous experiments: interleaved repeated A/B trials against the baseline with nonparametric statistics, plus scenario experiments (soak, burst, failure drills). Only statistically and practically significant winners get promoted — optionally behind a manual approval gate.

The name is the method: a single observation can't give you depth — two vantage points can. Cheap breadth-first screening and expensive depth-first validation together triangulate configurations that are *actually* better, not just lucky.

## Architecture in one paragraph

Parallax is a Kubernetes **operator** reconciling four CRDs — `Study`, `Trial`, `Plugin`, `Dataset` — plus a thin CLI (with a `--local` mode that runs the same controllers against kind, no install required). **Every extension seam is a versioned gRPC subprocess plugin**: targets, load drivers, metrics providers, search strategies, chaos scenarios, exporters, traffic captures. Plugins are declared by `Plugin` CRs, delivered as signed OCI images, verified before install, and **hot-reloaded** with drain-and-swap — the same battle-tested conventions as kapture's replay-engine ABI, generalized platform-wide. Results live in **PostgreSQL** (SQLite in local mode) with large artifacts in object storage, so every decision is auditable SQL, not a pile of files.

## How it relates to kapture

[Kapture](https://github.com/aburan28/kapture) captures real production traffic via Gateway API `RequestMirror` and replays it as fleet-wide, multi-cell load tests (`CaptureLoadTest`), with pluggable replay engines (builtin, k6, ghz). Parallax is the experimentation brain on top of that muscle:

- **Kapture provides**: realistic workloads (captured traffic as versioned datasets), distributed load generation (replay sharded across cells), and client-side run reports (counts, latency percentiles).
- **Parallax provides**: config-space definition and search, trial orchestration, metric collection and SLI evaluation, candidate selection, statistical validation experiments, durable results, reporting, and promotion.

Two first-party targets ship as plugins:

- **`target-kapture`** (dogfooding): tune capture-agent buffering, compression, HPA, and storage configurations — and calibrate the load-generation instrument you'll use for everything else.
- **`target-helm`**: patch Helm values or manifests of any service and benchmark each variant under replayed production traffic.

## Enterprise, without the asterisk

Apache-2.0 across platform, plugins, and charts — no open-core split. Signed images (cosign + SBOM + SLSA provenance), digest-pinned plugin verification, restricted-PSS security posture, least-privilege RBAC, NetworkPolicies, HA leader election, air-gap bundles, GitOps-native CRs, manual approval gates with recorded approver identity, and zero telemetry phone-home.

## Status

**M0 — operator + plugin scaffold — in progress.** The design is settled (see
[`docs/DESIGN.md`](docs/DESIGN.md)) and the M0 skeleton is landing:

- **CRDs** — `Study`, `Trial`, `Plugin`, `Dataset` (`parallax.dev/v1alpha1`) in [`api/v1alpha1/`](api/v1alpha1/).
- **Plugin ABI v1** — versioned gRPC contract (lifecycle + per-kind services: target, loaddriver, provider, strategy, scenario, exporter) in [`proto/plugin/v1/`](proto/plugin/v1/), with a Go plugin SDK in [`pkg/plugin/`](pkg/plugin/).
- **Results store** — a `Store` interface with **SQLite** (pure-Go, `CGO_ENABLED=0`) and **PostgreSQL** drivers, plus embedded SQL migrations in [`migrations/`](migrations/).
- **Operator + CLI + plugin host** — manager, `parallax` CLI (with a `--local` kind mode), subprocess plugin supervisor with fsnotify hot reload, and the four reconcilers.
- **Packaging** — Helm chart ([`charts/parallax/`](charts/parallax/)) and kustomize ([`config/`](config/)) with restricted-PSS security context, least-privilege RBAC, NetworkPolicies, HA leader election, and an optional bundled local Postgres.

M0 exit criterion: a 1-point `Study` runs end-to-end on kind (`--local`), SLI rows
land in the DB, and a report renders. See the roadmap in [`docs/DESIGN.md`](docs/DESIGN.md) §21.

## Building

Requires Go 1.25+, and (for the local environment) `kind`, `kubectl`, and `helm`.

```sh
make tools           # install pinned code-gen tools (buf, controller-gen, protoc-gen-*) into hack/bin
make generate        # regenerate the plugin ABI (buf) + deepcopy (controller-gen)
make manifests       # generate CRD YAML into config/crd and charts/parallax/crds
make build           # build the operator, CLI, plugin-installer, and first-party plugins (CGO_ENABLED=0)

hack/env-up.sh       # idempotent kind dev cluster: Gateway API + Envoy Gateway + MinIO + Prometheus (5s scrape)
export KUBECONFIG=hack/kind-kubeconfig

# Run a Study end-to-end in --local mode (same controllers against kind, SQLite store):
go run ./cmd/parallax apply --local examples/studies/capture-agent-throughput.yaml

hack/env-down.sh     # tear the cluster down
```

`make help` lists every target. To deploy the operator into an existing cluster,
install via the chart (`helm install parallax charts/parallax`) or kustomize
(`kubectl apply -k config/default`) — both ship the restricted-PSS posture,
least-privilege RBAC, and NetworkPolicies described in [`docs/DESIGN.md`](docs/DESIGN.md) §17.

## Documentation

- [`docs/DESIGN.md`](docs/DESIGN.md) — architecture, plugin ABI, CRDs, trial lifecycle, SLI providers, selection & validation methodology, results database, enterprise readiness, roadmap
- [`examples/studies/`](examples/studies/) — example `Study` CRs
- [`examples/plugins/`](examples/plugins/) — example `Plugin` catalog
- [`examples/datasets/`](examples/datasets/) — example `Dataset` registration

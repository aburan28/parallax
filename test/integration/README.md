# Integration tests (envtest)

Controller integration tests run against a real API server + etcd via
**envtest** (no kubelet, no containers) — the same infra kapture uses
(Appendix A.5). They exercise the reconcilers (agent C) end-to-end against the
CRDs: create a `Study`/`Trial`/`Plugin`/`Dataset`, assert phase transitions,
conditions, owner refs, finalizers, and the rows written to the store.

## Prerequisites

envtest needs the control-plane binaries (`kube-apiserver`, `etcd`, `kubectl`)
staged by `setup-envtest`:

```sh
# Install setup-envtest and stage k8s 1.31 binaries.
go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
export KUBEBUILDER_ASSETS="$(setup-envtest use 1.31.x --bin-dir "$(pwd)/hack/bin" -p path)"
```

## Running

```sh
make test-integration
# equivalently:
KUBEBUILDER_ASSETS="$(setup-envtest use 1.31.x -p path)" go test ./test/integration/... -race -count=1
```

The store under test is SQLite (pure-Go `modernc.org/sqlite`, `CGO_ENABLED=0`) in
a temp file, so integration runs need no external Postgres.

## What lands here (M1+)

- Study reconcile: `Pending → Sweeping` fan-out of Trials with owner refs and
  budget accounting; resume from the DB after a simulated restart (§15).
- Trial lifecycle gate ordering (Pending…Configuring…HealthGate…Warmup…Measuring…
  Draining…Collecting…Collected) with a fake plugin host.
- Plugin admission: verify/digest-resolution status, `namespaceSelector` gating.
- Dataset verification against a fixture manifest.

## Status

M0 scaffold: contract only. Suites are added with the controllers they cover.

<!-- TODO(m1): envtest suite for Study/Trial reconcile + DB resume -->
<!-- TODO(m2): Plugin admission + Dataset verification suites -->

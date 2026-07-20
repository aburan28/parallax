# Plugin conformance suites

Every plugin **kind** ships a conformance suite that runs against any plugin
binary and asserts it honors the ABI (DESIGN.md §5.6). Passing conformance is the
objective bar for the community catalog (§22) — there is no certification paywall.

```sh
# Run all first-party plugins through their kind's suite:
make conformance

# Or point the CLI at a single binary (any language, same ABI):
parallax plugin conformance ./bin/parallax-strategy-random
```

## What every suite checks (all kinds)

- **Handshake** — magic-cookie env in, exactly one `PARALLAX-PLUGIN|v1|unix|<sock>|grpc`
  line on stdout within `plugin.HandshakeTimeout`, then silence.
- **Describe** — reports its kind, ABI version, capabilities, and a valid JSON
  Schema for its config; version negotiation (N-1) behaves.
- **Configure** — fail-fast validation: good config accepted, malformed config
  rejected with a descriptive error (no panic, no partial state).
- **Drain** — `Lifecycle.Drain` returns within the grace window and the process
  exits cleanly; drain-and-swap hot reload is safe (§5.4).
- **Health** — the lifecycle health RPC reflects real readiness.

## Kind-specific checks

| Kind | Extra conformance |
|---|---|
| `strategy` | the PR #18 synthetic problems — see [`strategy/`](strategy/) |
| `provider` | canned query/window fixtures → expected SLI values |
| `target` | applies/patches against a fake cluster (envtest), health-gate semantics |
| `loaddriver` | maps a Study workload to a `CaptureLoadTest`/`TrafficReplay`, run-report shape |
| `scenario` | injects and reverts its fault within the declared window |
| `exporter` | emits the declared events with the recorded decision payload |

## Layout

```
test/conformance/
  README.md            # this file
  strategy/            # strategy suite + the PR #18 synthetic problems (§19)
  provider/  target/  loaddriver/  scenario/  exporter/   # (added M1–M2)
```

## Status

M0 scaffold: this directory documents the suite contract. The runnable Go suites
land alongside their kinds — strategies first in M1 (absorbing PR #18), providers
and targets with the sweep/select milestone. See `strategy/README.md`.

<!-- TODO(m1): implement the strategy conformance suite (Go), harness in test/conformance/strategy -->
<!-- TODO(m2): provider/target/loaddriver/scenario/exporter suites -->

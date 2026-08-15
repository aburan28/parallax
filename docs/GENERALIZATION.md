# Generalization review

*What it would take to run parallax with kapture entirely out of the picture, and to
benchmark systems that are not HTTP services.*

Reviewed against `main` @ `3d6fc72` (M1 sweep/select landed). Section references (§) are
to [`DESIGN.md`](DESIGN.md).

---

## 0. Verdict

The **architecture** is already generic: six plugin kinds behind a versioned gRPC ABI, an
opaque config blob per plugin, a store-agnostic analysis core, and a trial state machine
that only talks to the SUT through `target` RPCs. Nothing in the *shape* of the system
needs rethinking.

The **API surface and the wiring** are not generic. Two distinct problems:

1. **Kapture's data model is hard-coded in the CRDs, above the plugin seam.**
   `Study.spec.workloads[]` is a traffic-replay struct (`datasetRef` + `replay.{rate,
   distribution, abort}`), `Dataset.spec.captureRef` is a required kapture
   `TrafficCapture` reference, and the load-driver ABI returns an HTTP-latency-shaped
   `RunReport`. A load driver that is not kapture cannot express its own inputs or
   outputs — it has to pretend to be a replay engine.
2. **Most seams are declared but never called.** Of six plugin kinds, `scenario` and
   `exporter` are never invoked; `strategy` is bypassed by in-core substring matching on
   the plugin *name*; `target` uses 2 of its 5 RPCs and `loaddriver` 2 of 4. The seams
   that would make the platform extensible are, today, decorative — so "generic" cannot
   be demonstrated even where the ABI allows it.

Nine gaps follow, ranked. G1–G4 block a non-kapture user outright. G5–G8 block a healthy
third-party plugin ecosystem. G9 is cleanup.

### Scope: reading B — workload-agnostic

Two readings of "more generic" were on the table:

| Reading | Meaning |
| --- | --- |
| **A — kapture-optional** | Any HTTP/gRPC service, any load generator (k6, JMeter, Locust, wrk, a bespoke harness), any dataset source. Kapture becomes one plugin among several. |
| **B — workload-agnostic** | Also non-request/response systems: batch jobs, stream processors, databases (TPC-C/YCSB), training jobs, LLM eval harnesses. Adds dropping `RunReport`'s fixed fields entirely and decoupling the trial clock from "load start/stop". |

**B is the chosen scope.** §1's old non-goal — *"Not load testing arbitrary protocols. v1
speaks what kapture replays: HTTP and gRPC"* — has been replaced accordingly: parallax
models no protocol and no load shape at all.

### Status

**Phase 1 has landed** (schema + the clock work B requires). Gap headings below are marked
`LANDED` or `OPEN`; §3 tracks what remains.

The core no longer contains the words replay, rate, cell, or request. Concretely:
`Workload` is a driver reference plus an opaque config block; a workload declares whether
its window is closed by a duration or by the driver reporting done; the load-driver ABI
carries `map<string,double> metrics` instead of an HTTP run report; SLIs read from a
provider plugin, from the driver's own metrics, or from an in-core expression;
`Dataset` sources are discriminated with kapture as one kind; dimensions carry an explicit
target config path; and a default chart install grants no vendor API group.
`examples/studies/nightly-etl-runtime.yaml` is the proof: a batch ETL study with no
traffic, no latency percentile, and no kapture object anywhere in it.

---

## 1. Already generic — do not touch

Worth stating so the work stays scoped:

- `internal/analysis` — pure functions over `map[string]float64` SLIs; no provider, no
  protocol, no k8s (`select.go:33-50` is explicitly store-agnostic).
- `internal/stats`, `internal/space`, `internal/store` — the space resolver handles
  int/float/categorical with no target semantics; the store schema keys on
  `(run, config_hash, workload, rep, mode)`, all domain-neutral.
- The **lifecycle ABI** (`Describe`/`Configure`/`Health`/`Drain`) and the plugin host's
  handshake, hot-reload, and drain-and-swap. Correct and kind-agnostic.
- `Target` ABI (`proto/plugin/v1/target.proto`) — `Apply(config_hash, dimensions,
  raw_config_json)` / `Ready` / `Reset` / `Contract` is the right shape for any SUT.
- The `Plugin` CR (`api/v1alpha1/plugin_types.go`) — kind + OCI image + opaque config +
  cosign policy. Nothing kapture-specific.

The kapture references in `pkg/plugin/plugin.go`, `internal/space/space.go`, and the
proto headers are **comments crediting prior art**. They are fine; they are not coupling.

---

## 2. Gaps

Each gap below records **the state at review time** — the coupling, the evidence, and the
proposed change — with a status marker on the heading. Line references are to `3d6fc72`
and will drift; they are kept as the audit trail for why each change was made, not as a
map of the current tree.

### G1 — `Study.spec.workloads[]` is a traffic-replay struct  ·  `LANDED`

**Coupling.** `api/v1alpha1/study_types.go:137-184`. `Workload` requires `datasetRef` and
`replay`; `ReplaySpec` is annotated *"mirrors CaptureLoadTest.spec field names"* (`:150`)
and carries `rate.mode ∈ {Constant, OriginalTiming, Unlimited}` (`:162` — `OriginalTiming`
is meaningless outside recorded-traffic replay), `distribution.{cells, workersPerSpoke,
concurrencyPerWorker}` (kapture's fleet topology), and `abort.{maxDuration, errorPercent}`.

**Worse: the driver plugin is selected by `replay.engine`.**
`internal/controller/trial_controller.go:252-257` does
`pl.loadDriver = study.Spec.Workloads[i].Replay.Engine`. So the field documented as
kapture's *replay engine name* (`builtin` | `k6` | `ghz`) is in fact the **plugin name the
host dials**. Both shipped examples set `engine: builtin`, which resolves to a plugin
named `builtin` that does not exist; the call fails and is swallowed by `pluginSkipped`.
A k6 driver must be named `k6` at the CR level, and there is no way to run two studies
against differently-configured instances of the same driver.

**Why it blocks.** A JMeter driver, a YCSB driver, or a Kafka producer has no `cells`, no
`timeScale`, and no `requestsPerSecond`. Its parameters have nowhere to live: the CRD
prunes unknown fields, so users cannot pass them at all. The ABI is already right —
`StartRequest.workload_json` is opaque bytes — but the CRD refuses to carry an opaque
payload.

**Change.** Make the workload a driver reference plus an opaque config, exactly as
`Study.spec.target` already works:

```go
type Workload struct {
    Name   string    `json:"name"`
    Driver PluginRef `json:"driver"`                    // kind=loaddriver, Ready plugin
    // +optional — driver-defined; loaddriver-kapture accepts today's replay block verbatim
    Config *runtime.RawExtension `json:"config,omitempty"`
    // +optional — drivers that consume a registered corpus
    DatasetRef *LocalRef `json:"datasetRef,omitempty"`
    Warmup, Measure, Cooldown metav1.Duration `json:"...,omitempty"`
}
```

Keep `warmup`/`measure`/`cooldown` in core — they define the measurement window, which is
platform semantics, not driver semantics. Move `rate`/`distribution`/`abort` into
`loaddriver-kapture`'s config schema (publish it as a JSON Schema via `Describe()`, which
the ABI already supports). `datasetRef` becomes optional: a synthetic generator has no
dataset.

**Breaking?** Yes, `v1alpha1` schema change. Cheap now (two example studies, one M1
controller path), expensive after a v1beta1. Do it first.

---

### G2 — `Dataset` is a kapture capture, by definition  ·  `LANDED` (schema) / `OPEN` (verification)

**Coupling.** `api/v1alpha1/dataset_types.go:34-43`. `spec.captureRef` is **required** and
documented as *"the kapture TrafficCapture this dataset was recorded from"*;
`spec.preshard.shards` is kapture's `kapture-preshard` slicing.

**Why it blocks.** There is no way to register an S3 prefix of JSONL, a Git-versioned
query corpus, a database snapshot, a HAR file, or a generator seed. Any non-kapture
workload either abandons the `Dataset` CR (losing verification, the ID digest, and the
provenance the results DB records) or lies about a `TrafficCapture` that does not exist.

**Change.** Source-kind discrimination, with kapture as one kind:

```yaml
spec:
  source:
    kind: kapture-capture | object-storage | git | generator   # or a plugin name
    ref:  {name: prod-edge}          # kapture-capture
    uri:  s3://bucket/prefix/        # object-storage
    config: {...}                    # kind-defined, opaque
  storageRef: {name: minio}
  preshard: {shards: 8}              # optional; only kinds that support slicing
```

Verification (`status.idDigest`, `recordCount`, `manifest`) is exactly what
`DatasetReconciler` must delegate — either to the load driver that will consume the
dataset (add `Verify(dataset)` to the loaddriver ABI, since only the consumer knows the
format) or to a new `dataset` plugin kind. Delegating to the driver is the smaller change
and avoids a seventh kind.

**Breaking?** Yes, but `Dataset` has one example CR and a stub controller. Now is free.

---

### G3 — Strategy plugins are never called  ·  `OPEN` · **P0**

**Coupling.** `internal/controller/study_controller.go:193-216`:

```go
strat := strings.ToLower(study.Spec.Space.Strategy.Plugin)
if strings.Contains(strat, "random") { return randomPoints(...) }
// grid (default), sobol, asha — grid expansion until plugin-driven search lands
return space.GridPoints(sp)
```

The full `Strategy` ABI (`Init`/`Ask`/`Tell`/`Report`) exists, and
`cmd/plugins/strategy-{grid,random,sobol,asha}` are built and shipped — but no controller
ever dials a strategy plugin. `trialPlugins` (`internal/controller/trial_plugins.go:33-39`)
has no strategy method at all.

**Why it blocks.** "Bring your own search strategy" is the single most compelling
genericity claim in the design (§14, `strategy-optuna` in the plugin catalog), and it does
not work. Worse, the **substring match is a landmine**: a plugin named
`bayes-with-random-restarts` silently becomes the built-in random sampler, and every other
plugin name silently becomes grid — no error, no event, no status condition. A user gets
plausible-looking results from a strategy they did not choose.

**Change.** Two steps, both small:

1. **Immediately:** replace substring matching with exact names and an explicit builtin
   namespace (`builtin:grid`, `builtin:random`). Anything else that is not a Ready
   `strategy` plugin must fail the Study with a condition, not fall back silently.
2. **M1:** drive the ask/tell loop — `Init(space_json, budget_json, seed)` once per run,
   `Ask(count)` per sweep batch, `Tell(config_hash, objective, feasible)` as trials
   collect, `Report().done` to terminate. This also delivers the adaptive strategies
   (ASHA/Optuna) that the current one-shot expansion cannot express: today the whole space
   is materialized up front (`materializeSweep`), so no strategy can *react* to results.

**Breaking?** No API change. Behavioral, and the current behavior is arguably a bug.

---

### G4 — Dimension → config mapping has no home in the API  ·  `LANDED` (schema) / `OPEN` (admission)

**Coupling.** `api/v1alpha1/study_types.go:104-115`. A `Dimension` is `{name, int |
float | categorical}` — and §8 (`DESIGN.md:425`) states *"dimension names are
target-plugin paths"*. `target-kapture` knows that `agent.batchSize` means the
capture-agent flag; `target-helm` is supposed to map names to Helm value paths.

**The shipped generic example does not validate.**
`examples/studies/checkout-latency-bakeoff.yaml:27-35` writes:

```yaml
- name: gogc
  path: .env.GOGC          # ← no `path` field exists on Dimension
```

`Dimension` has no `Path`. The CRD prunes it, so `target-helm` receives a dimension named
`gogc` with no idea where it goes. The one example demonstrating a non-kapture target is
built on an API field that does not exist.

**Why it blocks.** Without an explicit mapping, every generic target must invent a naming
convention and every dimension name must double as a config path — which breaks the moment
a user wants a readable name, two targets in one study, or a path containing characters
that are awkward in a dimension name.

**Change.** Add an opaque per-target mapping to `Dimension` (`path string` covers Helm and
JSON-pointer targets; `mapping *runtime.RawExtension` covers the rest), pass it through
`ApplyRequest`, and **call `target.Contract()` at admission** so unknown dimensions fail
the Study rather than silently no-op'ing at trial time. `Contract` exists in the ABI and
`target-kapture` implements it (`cmd/plugins/target-kapture/dims.go`); nothing calls it,
and there is no admission webhook (`internal/webhook/` does not exist).

---

### G5 — The load-driver ABI hard-codes kapture's run report  ·  `LANDED`

**Coupling.** `proto/plugin/v1/loaddriver.proto:56-70`:

```proto
message RunReport {
  int64 total_requests / sent_requests / failed_requests / filtered_requests / duration_ms;
  double achieved_rps, mean_latency_ms, p50/p95/p99_latency_ms;
}
```

Annotated *"mirrors kapture's TrafficReplay run-report fields"*. These fields are also the
`provider-runreport` SLI surface — studies write `expr: p99LatencyMs`
(`checkout-latency-bakeoff.yaml:52-58`).

**Why it blocks.** A batch driver reports records/sec and job duration; a database driver
reports tpmC and per-transaction percentiles; a training-job driver reports
steps/sec. None fit. `filtered_requests` is a kapture concept no other driver has. **[B]**

**Change.** Keep the typed fields as an optional well-known message for HTTP/gRPC drivers
(they are genuinely useful and `provider-runreport` depends on them), and add the open
form alongside:

```proto
message StopResponse {
  bool ok = 1;
  RunReport report = 2;                 // optional, request/response drivers
  map<string, double> metrics = 3;      // driver-defined, always populated
  bytes report_json = 4;                // full fidelity for artifacts/exports
}
```

`provider-runreport` then resolves `expr` against `metrics` first, falling back to
`RunReport` field names — so today's studies keep working and a new driver just publishes
its own metric names.

**Two wiring bugs in the same seam, worth fixing together:**

- **`run_ref` is discarded.** `StartResponse.run_ref` is documented as *"opaque handle for
  Watch/Stop"*, but `trial_controller.go:188-190` calls `Stop` with `RunRef:
  string(trial.UID)`. The constant intended to carry it —
  `loadRunRefAnnotation` (`trial_controller.go:53-55`) — is **declared and never used**.
  Every driver is therefore forced to key its runs by trial UID, contradicting its own
  ABI. Persist the returned handle on the annotation and pass it back.
- **`Watch` is never called.** No caller anywhere in `internal/`. Consequently
  `workloads[].replay.abort.{maxDuration, errorPercent}` is **inert** — the abort policy
  the CRD advertises is not enforced by the core, and the driver's progress stream
  (`sent`, `failed`, `achieved_rps`, `aborted`) never reaches the trial. This is also why
  `warmup`/`measure` durations are ignored: `Warmup → Measuring → Draining` transitions
  fire on the 2s requeue, so `TrialSpec.Fidelity` is written by the Study controller
  (`study_controller.go:249-253`) and read by nobody.

---

### G6 — `SLISpec` conflates provider, dialect, and class  ·  `LANDED` (schema) / `OPEN` (derived eval)

**Coupling.** `api/v1alpha1/study_types.go:186-199` — `{name, provider, query, expr,
class}`. Two query dialects are enumerated in the core API: `query` for PromQL-ish
providers, `expr` for status-scraping providers. A third provider dialect (say, a SQL
provider, or one needing `{metric, aggregation, filters}`) has no field.

**And `derived` is both a provider and a class, inconsistently.** The controller skips on
`sli.Class == "derived"` (`trial_controller.go:399`), while both examples write
`provider: derived` with no class (`checkout-latency-bakeoff.yaml:70-72`). A study written
the documented way makes the core try to dial a plugin named `derived`, fail, and record
the SLI as `0` — which then flows into guardrails and Pareto ranking as a real value.
(Separately: `collectAndPersist` records `0` for *any* unreachable provider
(`trial_controller.go:305-320`) with `ok` discarded, so a dead provider silently produces
a config that looks perfect on a `minimize` objective. Worth fixing regardless of
genericity.)

**Change.**

```go
type SLISpec struct {
    Name  string `json:"name"`
    Class string `json:"class,omitempty"`     // fidelity | client | server | derived
    // exactly one of:
    From    *SLISource `json:"from,omitempty"`     // {provider, config: RawExtension}
    Derived *DerivedSLI `json:"derived,omitempty"` // {expr} — evaluated in-core
}
```

One opaque config per provider (mirroring `Plugin.spec.config`), `derived` promoted to a
first-class in-core kind rather than a magic provider name, and `query`/`expr` retained as
deprecated shorthands that the controller folds into `From.Config` for one release.

---

### G7 — Half the extension seams are never exercised  ·  `OPEN` · **P1**

| Kind | ABI RPCs | Called by core | Effect |
| --- | --- | --- | --- |
| `target` | Prepare, Apply, Ready, Reset, Contract | **Apply, Ready** | No per-study prep; **no `Reset` between trials** → trial *n* inherits trial *n−1*'s state (caches, HPA scale, storage), which quietly undermines the comparison the whole platform exists to make. No `Contract` → unknown dimensions surface as silent no-ops. |
| `loaddriver` | Plan, Start, Watch, Stop | **Start, Stop** | No pre-flight validation; abort policy inert (G5). |
| `provider` | Capabilities, Collect, Snapshot | **Collect** | No capability negotiation (core cannot tell whether a provider supports a class before asking); no artifact snapshots. |
| `strategy` | Init, Ask, Tell, Report | **none** | G3. |
| `scenario` | — | **none** | `validation.scenarios: [soak-30m, burst-3x]` is accepted by the CRD and ignored. |
| `exporter` | — | **none** | `promotion.output` is accepted and ignored. |

Some of this is honest M0/M1 sequencing. It matters for *this* review because **an
extension point nobody calls cannot be validated as generic** — the first third-party
plugin author will discover the seam's real shape, not the documented one. The
`test/conformance/` harness (currently a README for strategies) is the right place to pin
each kind's contract; a conformance suite that passes against a deliberately non-kapture
fake driver is the cheapest proof that G1–G5 actually landed.

---

### G8 — Plugins inherit the operator's identity, so their RBAC lives in the core chart  ·  `LANDED` (step 1) / `OPEN` (steps 2–3)

**Coupling.** Plugins are `exec.Command` subprocesses in the manager pod
(`internal/pluginhost/process.go:53-60`) — same ServiceAccount, same NetworkPolicy, same
PSS context, same secrets mount. So a plugin's cluster permissions can only come from the
operator's ClusterRole, and the base chart therefore ships kapture's CRD permissions to
**every** installation:

```yaml
# charts/parallax/templates/rbac.yaml:70-81 (and config/rbac/role.yaml:51-56)
- apiGroups: ["capture.gateway.io"]
  resources: [captureloadtests, trafficreplays, trafficcaptures]
  verbs: [get, list, watch, create, update, patch, delete]
```

An org that never installs kapture still grants write access to CRDs it does not have, and
a `least-privilege RBAC` claim in the README is weakened by an unrelated vendor's API
group. Conversely, a third-party plugin (a Terraform target, an AWS load driver) **cannot
obtain permissions at all** without a PR to the core chart — which is the opposite of a
plugin ecosystem.

This also collides with §5.5's stated boundary (*"no ambient credentials — plugins get no
secrets by default"*): today a plugin subprocess can read the operator's SA token off
disk. The mediation described in the design does not exist yet. (Incidentally,
`charts/parallax/templates/serviceaccount.yaml:13` references
`.Values.serviceAccount.automountServiceAccountToken`, which is **not defined** in
`values.yaml` — it renders as `null`, so the token is mounted by API-server default. The
operator needs it; the point is that no one chose it.)

**Change, cheapest first:**

1. **Now:** move the `capture.gateway.io` block out of the base ClusterRole into an opt-in
   chart value (`rbac.extraRules` / a `parallax-plugin-kapture` ClusterRole shipped with
   the kapture plugin). Default install: no vendor API groups.
2. **M2:** let a `Plugin` CR declare the rules it needs (`spec.rbac.rules`), and have the
   Plugin controller reconcile a per-plugin ClusterRole — reviewable, auditable, and
   scoped by the same `namespaceSelector` that already gates plugin use.
3. **Longer term:** an out-of-pod transport. The ABI is gRPC already; a plugin running as
   its own Deployment with its own SA needs only a `transport: {unix | tcp}` switch in the
   host and a mTLS story. Worth designing toward even if it is not built soon — it is the
   only real answer for untrusted third-party plugins.

---

### G9 — Local environment, naming, and the k8s assumption  ·  `OPEN` · **P2**

- `internal/env/env.go:31-84` enumerates the local stack as a fixed list: kapture, Envoy
  Gateway, MinIO, prom-lite — all `Optional: false`, including the SUT itself. `--local`
  therefore means "kapture's dev environment," not "parallax's." Make it profile-driven
  (`--profile minimal|kapture|byo`), with only the store and a metrics endpoint required.
- The two shipped studies are both replay studies against kapture-provided load; the
  "generic" one (`checkout-latency-bakeoff`) does not validate (G4) and still depends on
  kapture for load. **A study using neither kapture CRDs nor captured traffic is the
  single most useful artifact this work can produce** — see §4.
- `charts/parallax/values.yaml:99,112` document DB defaults as *"mirrors kapture
  history.localPostgres"*. Harmless, but it reads as a subsystem of kapture rather than a
  peer.
- **k8s itself.** Every entry point is a CRD reconcile; `--local` still requires kind. The
  trial state machine only touches the SUT through plugin RPCs, so a non-k8s driver loop
  is *architecturally* reachable — the blockers are `controller-runtime` ownership of the
  loop and `EnvironmentSpec.ClusterRef` being kubeconfig-only
  (`study_types.go:82-86`). I would **not** chase this now: it is a large lift for a
  speculative audience, and G1–G8 deliver most of the genericity value while staying
  k8s-native. Flagging it so the CRD-vs-config-file boundary is a conscious choice.

---

## 3. Sequencing

**Phase 1 — schema and the trial clock. `LANDED`.** G1 workload/driver split · G2 dataset
sources · G4 dimension paths · G5 open driver metrics + `run_ref` + `Progress` · G6 SLI
source split (with `driver` as a third source) · the [B] clock work: warmup, measure,
cooldown and `maxDuration` are all honored, and `completion: driver` lets a
run-to-completion workload end its own window · G8 step 1 (vendor RBAC out of the base
install) · trials materialize per workload rather than only `workloads[0]`.

Two things were removed rather than ported. `provider-runreport` is gone: its job was to
re-read kapture CR statuses for metrics the core already holds after `Stop`, so driver
metrics became a first-class SLI source instead (`slis[].driver`). The streaming
`LoadDriver.Watch` RPC is gone in favour of unary `Progress`, because a reconcile loop
cannot consume a stream — the streaming seam had no possible caller, which is why the
abort policy was inert.

**Phase 2 — the remaining seams. `OPEN`.**
- **G3, the highest-value item left:** drive the strategy ask/tell loop, and immediately
  replace the substring match on plugin names with exact `builtin:grid` / `builtin:random`
  plus a hard failure for an unknown strategy. Until this lands, "bring your own search"
  does not work and an unrecognized strategy silently becomes grid.
- **G7:** `target.Reset` between trials (today trial *n* inherits trial *n−1*'s state);
  `target.Contract` at admission, which needs the validating webhook that does not yet
  exist (G4's admission half); `loaddriver.Plan` pre-flight; provider `Capabilities`;
  scenario and exporter wiring.
- **G6 remainder:** derived-SLI expression evaluation. The schema is correct now
  (`slis[].derived.expr`) but nothing evaluates it, so derived SLIs resolve as not-ok.
  This wants the same expression engine as `space.constraints`, which is also unevaluated
  — one dependency, two features.
- **G2 remainder:** delegate dataset verification to the driver that consumes the corpus;
  `status.idDigest` is still a placeholder derived from the source identity.

**Phase 3 — ecosystem. `OPEN`.** G8 steps 2–3 (per-plugin ClusterRoles declared by the
`Plugin` CR; out-of-pod plugin transport) · conformance suites per kind, run against
deliberately non-kapture fakes · G9.

**Explicit non-goals.** Do not generalize the results-DB schema (already domain-neutral),
the statistics core, the plugin handshake/hot-reload machinery, or the operator model.

## 4. Definition of done

One acceptance test settles the whole question — a study that:

- targets a plain Deployment via `target-helm` (no kapture chart, no `capture.gateway.io`
  object created at any point);
- drives load with a **non-kapture driver**;
- registers its corpus as a `Dataset` with `source.kind: object-storage`;
- reads SLIs from `provider-prometheus` plus in-core sources;
- searches with a `strategy` **plugin**, not a builtin;
- runs green on a kind cluster whose install has **no kapture CRDs and no
  `capture.gateway.io` RBAC**.

`examples/studies/nightly-etl-runtime.yaml` is that study, and it goes further than the
original bar: its workload is a batch ETL job, so there is no traffic and no latency
percentile anywhere in it. Every clause above is now *expressible*, and the last one is
true by default — `helm template charts/parallax` emits no `capture.gateway.io` rule
unless `rbac.plugins.kapture=true`.

What the study still needs to actually *run* green: a real `loaddriver-batchjob`
(first-party drivers are M0 skeletons), G3 so `strategy-sobol` is genuinely consulted
rather than silently expanded as a grid, and G6's derived evaluation so
`core_seconds_per_million_records` resolves. Those are Phase 2, and they are now the only
things between this file and a green run — not schema.

Guarding the bar: `api/v1alpha1/examples_test.go` decodes every shipped example strictly
against the typed API. The original review found `checkout-latency-bakeoff.yaml` using a
`dimensions[].path` field that did not exist — the API server prunes unknown fields, so it
looked fine and did nothing. Strict decoding makes that class of drift a test failure.

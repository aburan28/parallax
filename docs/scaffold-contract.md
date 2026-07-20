# M0 Scaffold Contract (internal — build coordination)

This file is the binding contract for the M0 scaffold build. It fixes the module
path, versions, package layout, and the shared Go interfaces so that independently
authored subtrees compile together. **Read it fully before writing any file.**

> This is a working document for the scaffold effort, not user-facing docs. The
> canonical design is `docs/DESIGN.md`.

## Hard rules for contributors (human or agent)

1. **Do NOT edit `go.mod` or `go.sum`.** Dependency versions are fixed (below). If you
   believe you need a new dependency, note it in a `// TODO(dep: <module>)` comment and
   use only the standard library plus the already-listed modules.
2. **Do NOT run `go build`, `go test`, `go mod tidy`, `go install`, `buf`, or
   `controller-gen`.** A single integration pass runs these centrally after all subtrees
   land. Your job is to *write source files* that compile against the contract.
3. **Write only within your assigned directories.** Never modify `api/v1alpha1/*`,
   `proto/plugin/v1/*`, `pkg/plugin/*`, `internal/store/store.go`, `internal/store/models.go`,
   `internal/pluginhost/host.go`, `migrations/*`, `internal/version/*`, or any foundation
   file — these are frozen. You may *add* new files to your directories.
4. **Every `.go` file starts with the Apache license header** (copy it verbatim from any
   existing file, e.g. `internal/version/version.go`).
5. **Code must compile against the frozen interfaces exactly as specified here.** Do not
   invent alternate signatures. If a type you need is missing, add it in your own package.
6. Keep implementations real but minimal (M0 skeleton): correct wiring and honest
   `TODO(m1)`/`TODO(m2)` markers where a full implementation is out of M0 scope. No panics
   on the happy path; return descriptive errors instead.

## Environment (informational — the integration pass handles this)

- Toolchain binaries (`buf`, `controller-gen`, `protoc-gen-go*`) live in `/Users/adamburan/go/bin` (apfs).
- The repo is on an **exFAT volume that creates `._*` AppleDouble sidecars**; Go ignores
  `._*` files, so they don't affect builds. Do not commit them.

## Module and pinned versions

- Module path: `github.com/aburan28/parallax`
- Go: `1.25.0`
- Frozen direct dependencies (already in `go.mod`):
  `sigs.k8s.io/controller-runtime v0.23.1`, `k8s.io/api|apimachinery|client-go v0.35.1`,
  `k8s.io/apiextensions-apiserver v0.35.0`, `google.golang.org/grpc v1.79.2`,
  `google.golang.org/protobuf v1.36.11`, `github.com/spf13/cobra v1.10.2`,
  `github.com/spf13/pflag v1.0.10`, `github.com/jackc/pgx/v5 v5.10.0`,
  `modernc.org/sqlite v1.54.0` (pure-Go, **CGO_ENABLED=0**), `github.com/fsnotify/fsnotify v1.9.0`,
  `github.com/go-logr/logr v1.4.3`, `github.com/prometheus/client_golang v1.23.2`,
  `github.com/google/uuid v1.6.0`, `sigs.k8s.io/yaml v1.6.0`.

## Package / import map

| Import path | Role | Owner |
|---|---|---|
| `github.com/aburan28/parallax/api/v1alpha1` | CRD types (Study/Trial/Plugin/Dataset) | FROZEN |
| `github.com/aburan28/parallax/proto/plugin/v1` (`pluginv1`) | generated ABI | FROZEN |
| `github.com/aburan28/parallax/pkg/plugin` | plugin Go SDK | FROZEN |
| `github.com/aburan28/parallax/internal/pluginhost` | subprocess supervisor | host_impl.go = agent B |
| `github.com/aburan28/parallax/internal/store` | Store interface + models (FROZEN) | impls = agent A |
| `github.com/aburan28/parallax/internal/store/sqlite` | SQLite driver | agent A |
| `github.com/aburan28/parallax/internal/store/postgres` | Postgres driver | agent A |
| `github.com/aburan28/parallax/internal/controller` | 4 reconcilers | agent C |
| `github.com/aburan28/parallax/internal/{space,stats,analysis,artifacts,report,env}` | support libs | agent F |
| `github.com/aburan28/parallax/internal/version` | build stamp (FROZEN) | — |
| `github.com/aburan28/parallax/migrations` | embedded SQL (FROZEN) | — |
| `github.com/aburan28/parallax/cmd/parallax-operator` | manager main | agent D |
| `github.com/aburan28/parallax/cmd/parallax` | CLI | agent D |
| `github.com/aburan28/parallax/cmd/plugin-installer` | OCI→/plugins installer | agent E |
| `github.com/aburan28/parallax/cmd/plugins/*` | first-party plugin mains | agent E |

## Frozen interface: `internal/store`

```go
type Store interface {
    Migrate(ctx context.Context) error
    Ping(ctx context.Context) error
    Close() error
    UpsertStudy(ctx context.Context, s *StudyRecord) (int64, error)
    CreateRun(ctx context.Context, r *RunRecord) (int64, error)
    UpdateRunPhase(ctx context.Context, runID int64, phase string) error
    UpsertTrial(ctx context.Context, t *TrialRecord) (int64, error)
    InsertSLIValues(ctx context.Context, values []SLIValueRecord) error
    InsertObservation(ctx context.Context, o *ObservationRecord) error
    InsertArtifact(ctx context.Context, a *ArtifactRecord) error
    InsertDecision(ctx context.Context, d *DecisionRecord) (int64, error)
    InsertValidation(ctx context.Context, v *ValidationRecord) error
    InsertScenario(ctx context.Context, s *ScenarioRecord) error
    InsertPromotion(ctx context.Context, p *PromotionRecord) error
    UpsertDataset(ctx context.Context, d *DatasetRecord) error
    RecordPluginAudit(ctx context.Context, a *PluginAuditRecord) error
    GetRun(ctx context.Context, runID int64) (*RunRecord, error)
    ListTrialsForRun(ctx context.Context, runID int64) ([]TrialRecord, error)
    ListSLIValuesForRun(ctx context.Context, runID int64) ([]SLIValueRecord, error)
}

// Drivers self-register: store.Register("sqlite", Open) in an init().
// store.Open(ctx, dsn) dispatches on the DSN scheme.
```

Records are in `internal/store/models.go` (StudyRecord, RunRecord, TrialRecord,
SLIValueRecord, ObservationRecord, DecisionRecord, ValidationRecord, ScenarioRecord,
PromotionRecord, ArtifactRecord, DatasetRecord, PluginAuditRecord). Embedded migrations:
`migrations.FS` (subdirs `postgres/`, `sqlite/`), `migrations.Dialect(scheme) string`.

**Agent A** writes `internal/store/sqlite/sqlite.go` and `internal/store/postgres/postgres.go`,
each with an `Open(ctx, dsn) (store.Store, error)`, an `init(){ store.Register(...) }`,
a concrete type implementing every `store.Store` method, and a `Migrate` that applies
the embedded SQL for its dialect. SQLite uses `modernc.org/sqlite` (driver name `sqlite`),
Postgres uses `github.com/jackc/pgx/v5/stdlib` (driver name `pgx`) via `database/sql`, or
`pgxpool` directly. Parse the DSN (`sqlite:///path` → file path; `postgres://...` → passthrough).

## Frozen SDK: `pkg/plugin`

```go
const ( MagicEnv, MagicValue, SocketDirEnv, HandshakePrefix, ABIVersion, HandshakeTimeout, DefaultDrainGrace )
func Serve(cfg ServeConfig) error         // plugins call this from main()
func ParseHandshake(line string) (Handshake, error)   // host side
func Dial(ctx, Handshake) (*grpc.ClientConn, error)   // host side
func FormatHandshake(socketPath string) string
func KindString(pluginv1.PluginKind) string
type ServeConfig struct { Name string; Kind pluginv1.PluginKind; Lifecycle pluginv1.LifecycleServer;
    Target pluginv1.TargetServer; LoadDriver pluginv1.LoadDriverServer; Provider pluginv1.ProviderServer;
    Strategy pluginv1.StrategyServer; Scenario pluginv1.ScenarioServer; Exporter pluginv1.ExporterServer }
type BaseLifecycle struct { pluginv1.UnimplementedLifecycleServer; Name string; Kind pluginv1.PluginKind;
    ConfigSchema string; Capabilities []string; ConfigureFunc func([]byte)(bool,string);
    HealthFunc func()(bool,string); DrainFunc func()error }   // ready-to-embed Lifecycle
```

A **plugin main** looks like (agent E, one per `cmd/plugins/*`):

```go
func main() {
    impl := &server{}
    cfg := plugin.ServeConfig{
        Name: "prometheus", Kind: pluginv1.PluginKind_PLUGIN_KIND_PROVIDER,
        Lifecycle: &plugin.BaseLifecycle{Name: "prometheus", Kind: pluginv1.PluginKind_PLUGIN_KIND_PROVIDER},
        Provider: impl,
    }
    if err := plugin.Serve(cfg); err != nil { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
}
```
where `server` embeds `pluginv1.UnimplementedProviderServer` and implements the kind RPCs.

## Frozen interface: `internal/pluginhost`

```go
type Host interface {
    Ensure(ctx context.Context, p *v1alpha1.Plugin) error
    Unload(ctx context.Context, name string) error
    Conn(name string) (*grpc.ClientConn, error)
    Ready(name string) bool
    Digest(name string) (string, bool)
    Close(ctx context.Context) error
}
type Options struct { PluginDir, SocketDir string; HostABIVersions []string; HealthIntervalSeconds int; SkipVerify bool }
```

**Agent B** writes `internal/pluginhost/host_impl.go` (+ helpers): a `New(opts Options) Host`
constructor and a concrete type. Responsibilities (M0): discover `parallax-<kind>-<name>`
in PluginDir; launch with `PARALLAX_PLUGIN_MAGIC`/`PARALLAX_PLUGIN_SOCKET_DIR` env; read+parse
the stdout handshake with `plugin.ParseHandshake` (timeout `plugin.HandshakeTimeout`); `plugin.Dial`;
Describe/Configure; a health supervisor goroutine; fsnotify-watch PluginDir (debounce ~500ms) and
drain-and-swap on change (call Lifecycle.Drain, launch new, then swap); cosign verify is a stub
gated by `Options.SkipVerify` with a `TODO(m2)`. Keep a map name→{proc, conn, digest, ready}.

## Frozen types: `api/v1alpha1`

CRDs: `Study{Spec StudySpec; Status StudyStatus}`, `Trial{Spec TrialSpec; Status TrialStatus}`,
`Plugin{Spec PluginSpec; Status PluginStatus}` (cluster-scoped), `Dataset{...}`. Phase constants:
`StudyPhase*`, `TrialPhase*` (Pending…Configuring…HealthGate…Warmup…Measuring…Draining…Collecting…
Collected|Failed|Aborted|Invalid), `PluginPhase*`, `DatasetPhase*`, `PluginKind*` (target/loaddriver/
provider/strategy/scenario/exporter), `TrialMode*`. `PluginRef{Plugin string; Config *runtime.RawExtension}`.
Register via `v1alpha1.AddToScheme`. Study/Trial/Dataset are namespaced; Plugin is cluster-scoped.
See the type files for exact field names — match them exactly.

## Cross-subtree symbols (pin these exact names)

To let the operator main and CLI (agent D) wire controllers (agent C) without knowing
their internals, **agent C must provide** in `internal/controller/`:

```go
// Deps carries the shared collaborators every reconciler needs.
type Deps struct {
    Store store.Store
    Host  pluginhost.Host
}
// Reconciler types (each with SetupWithManager(mgr ctrl.Manager) error):
type StudyReconciler   struct { client.Client; Scheme *runtime.Scheme; Recorder record.EventRecorder; Deps }
type TrialReconciler   struct { client.Client; Scheme *runtime.Scheme; Recorder record.EventRecorder; Deps }
type PluginReconciler  struct { client.Client; Scheme *runtime.Scheme; Recorder record.EventRecorder; Deps }
type DatasetReconciler struct { client.Client; Scheme *runtime.Scheme; Recorder record.EventRecorder; Deps }

// SetupAll registers all four reconcilers on the manager. Agent D calls ONLY this.
func SetupAll(mgr ctrl.Manager, deps Deps) error
```

**Agent D** (operator main) does: build scheme (`v1alpha1.AddToScheme`), create manager,
`store.Open`, `pluginhost.New`, then `controller.SetupAll(mgr, controller.Deps{Store: st, Host: host})`,
then `mgr.Start`. The CLI's `--local` path may reuse `SetupAll` against a kind kubeconfig with a
SQLite store.

## Conventions

- License header on every `.go` file.
- Logging: controller-runtime `logf "sigs.k8s.io/controller-runtime/pkg/log"`; get a logger with
  `logf.FromContext(ctx)`. No `fmt.Println` in library/controller code (plugins may print the one
  handshake line only, via the SDK).
- Errors: wrap with `fmt.Errorf("...: %w", err)`.
- Controllers: standard kubebuilder reconciler shape (`SetupWithManager`, `Reconcile`), owner refs
  (Study owns Trial), finalizers where cleanup is needed, `.status.conditions` via `meta.SetStatusCondition`,
  Events via `record.EventRecorder`. Add `// +kubebuilder:rbac` markers.
- Contexts flow through every call; never `context.Background()` inside a reconcile.

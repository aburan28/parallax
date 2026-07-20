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

package pluginhost

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/go-logr/logr"
	"google.golang.org/grpc"
	"k8s.io/apimachinery/pkg/runtime"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/aburan28/parallax/api/v1alpha1"
	"github.com/aburan28/parallax/pkg/plugin"
	pluginv1 "github.com/aburan28/parallax/proto/plugin/v1"
)

// healthRPCTimeout bounds a single supervisor Health probe.
const healthRPCTimeout = 5 * time.Second

// entry is the supervisor's record for one loaded plugin subprocess. All mutable
// fields (ready, degraded, conn, digest, abi) are guarded by host.mu.
type entry struct {
	name       string // the Plugin CR name and the map key (e.g. "provider-prometheus")
	kind       string // spec.Kind string (e.g. "provider")
	binName    string // resolved binary basename (e.g. "parallax-provider-prometheus")
	binPath    string // absolute path under PluginDir
	image      string // OCI image ref from spec (for the M2 cosign digest)
	configJSON []byte // last config bytes sent via Configure (retained for hot-reload)

	cmd       *exec.Cmd
	conn      *grpc.ClientConn
	handshake plugin.Handshake

	digest   string
	abi      string
	ready    bool
	degraded bool
}

// launchSpec is the input to a single process launch (a fresh load or a hot-reload swap).
type launchSpec struct {
	name       string
	kind       string
	binName    string
	binPath    string
	image      string
	configJSON []byte
}

// host is the concrete subprocess plugin supervisor. It launches plugin binaries,
// performs the stdout handshake, negotiates the ABI, runs a health supervisor loop,
// and hot-reloads binaries with drain-and-swap. It is safe for concurrent use.
type host struct {
	opts Options
	log  logr.Logger

	mu      sync.RWMutex
	entries map[string]*entry
	closed  bool

	// launchMu serializes launches (fresh loads and reload swaps) so two callers
	// never race to start the same plugin twice.
	launchMu sync.Mutex

	watcher *fsnotify.Watcher
	stopCh  chan struct{}
	wg      sync.WaitGroup
}

var _ Host = (*host)(nil)

// New returns a Host with the M0 defaults applied. It starts the health supervisor
// goroutine and, when PluginDir is watchable, the fsnotify hot-reload goroutine.
func New(opts Options) Host {
	if opts.PluginDir == "" {
		opts.PluginDir = "/plugins"
	}
	if opts.SocketDir == "" {
		opts.SocketDir = os.TempDir()
	}
	if len(opts.HostABIVersions) == 0 {
		opts.HostABIVersions = []string{plugin.ABIVersion}
	}
	if opts.HealthIntervalSeconds <= 0 {
		opts.HealthIntervalSeconds = 10
	}

	h := &host{
		opts:    opts,
		log:     logf.Log.WithName("pluginhost"),
		entries: make(map[string]*entry),
		stopCh:  make(chan struct{}),
	}

	// Hot-reload watcher. A missing/unwatchable PluginDir is non-fatal: hot-reload
	// is simply disabled until the operator restarts once the volume is present.
	// TODO(m1): re-add the watch if PluginDir appears later (retry loop).
	if w, err := fsnotify.NewWatcher(); err != nil {
		h.log.Error(err, "failed to create fsnotify watcher; hot-reload disabled")
	} else {
		h.watcher = w
		if err := w.Add(opts.PluginDir); err != nil {
			h.log.Error(err, "failed to watch plugin dir; hot-reload disabled", "dir", opts.PluginDir)
		} else {
			h.wg.Add(1)
			go h.watchLoop()
		}
	}

	h.wg.Add(1)
	go h.healthLoop()

	return h
}

// Ensure installs/loads (idempotently) the plugin described by a Plugin CR and drives
// it to Ready, updating the passed-in CR's status fields on success.
func (h *host) Ensure(ctx context.Context, p *v1alpha1.Plugin) error {
	if p == nil {
		return fmt.Errorf("pluginhost: nil Plugin")
	}
	log := logf.FromContext(ctx).WithName("pluginhost")

	kind := string(p.Spec.Kind)
	if kind == "" {
		return fmt.Errorf("plugin %q: spec.kind is empty", p.Name)
	}
	// shortName = CR name with a leading "<kind>-" stripped, e.g.
	// "provider-prometheus" -> "prometheus"; binary is "parallax-<kind>-<shortName>".
	shortName := strings.TrimPrefix(p.Name, kind+"-")
	binName := fmt.Sprintf("parallax-%s-%s", kind, shortName)
	binPath := filepath.Join(h.opts.PluginDir, binName)

	h.launchMu.Lock()
	defer h.launchMu.Unlock()

	// Idempotent fast-path: already loaded ⇒ just refresh status.
	h.mu.RLock()
	existing, ok := h.entries[p.Name]
	h.mu.RUnlock()
	if ok {
		// TODO(m1): if p.Spec.Config changed since launch, re-Configure or, if the
		// plugin can't reconfigure live, drain-and-swap to apply the new config.
		h.applyStatus(p, existing)
		return nil
	}

	spec := launchSpec{
		name:       p.Name,
		kind:       kind,
		binName:    binName,
		binPath:    binPath,
		image:      p.Spec.Image,
		configJSON: rawConfig(p.Spec.Config),
	}

	newEntry, err := h.launch(ctx, spec)
	if err != nil {
		p.Status.Phase = v1alpha1.PluginPhaseFailed
		return fmt.Errorf("ensure plugin %q: %w", p.Name, err)
	}

	h.mu.Lock()
	h.entries[p.Name] = newEntry
	h.mu.Unlock()

	h.applyStatus(p, newEntry)
	log.Info("plugin ready", "plugin", p.Name, "bin", binName, "abi", newEntry.abi, "digest", newEntry.digest)
	return nil
}

// applyStatus copies a loaded entry's negotiated fields onto the CR status.
func (h *host) applyStatus(p *v1alpha1.Plugin, e *entry) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if e.degraded {
		p.Status.Phase = v1alpha1.PluginPhaseDegraded
	} else {
		p.Status.Phase = v1alpha1.PluginPhaseReady
	}
	// TODO(m2): ResolvedDigest must be the cosign-verified image digest (§5.3, §9);
	// for M0 it is a sha256 of the on-disk binary (see resolveDigest).
	p.Status.ResolvedDigest = e.digest
	p.Status.ABIVersion = e.abi
}

// Unload drains (bounded grace) and stops a plugin by CR name. Idempotent.
func (h *host) Unload(ctx context.Context, name string) error {
	h.mu.Lock()
	e, ok := h.entries[name]
	if ok {
		delete(h.entries, name)
	}
	h.mu.Unlock()
	if !ok {
		return nil
	}
	h.drainAndTerminate(ctx, e)
	logf.FromContext(ctx).WithName("pluginhost").Info("plugin unloaded", "plugin", name)
	return nil
}

// Conn returns the gRPC conn for a Ready plugin.
func (h *host) Conn(name string) (*grpc.ClientConn, error) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	e, ok := h.entries[name]
	if !ok {
		return nil, fmt.Errorf("pluginhost: plugin %q is not loaded", name)
	}
	if e.conn == nil {
		return nil, fmt.Errorf("pluginhost: plugin %q has no connection", name)
	}
	if !e.ready || e.degraded {
		return nil, fmt.Errorf("pluginhost: plugin %q is not Ready", name)
	}
	return e.conn, nil
}

// Ready reports whether a plugin is loaded and healthy.
func (h *host) Ready(name string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	e, ok := h.entries[name]
	return ok && e.ready && !e.degraded
}

// Digest returns the resolved digest of a loaded plugin.
func (h *host) Digest(name string) (string, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	e, ok := h.entries[name]
	if !ok {
		return "", false
	}
	return e.digest, true
}

// Close stops the supervisor goroutines and drains+stops every plugin. Idempotent.
func (h *host) Close(ctx context.Context) error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	pending := make([]*entry, 0, len(h.entries))
	for _, e := range h.entries {
		pending = append(pending, e)
	}
	h.entries = make(map[string]*entry)
	h.mu.Unlock()

	close(h.stopCh)
	if h.watcher != nil {
		_ = h.watcher.Close()
	}
	for _, e := range pending {
		h.drainAndTerminate(ctx, e)
	}
	h.wg.Wait()
	return nil
}

// healthLoop is the supervisor: it probes Lifecycle.Health on every loaded plugin
// each HealthIntervalSeconds and marks failing plugins degraded (never crashes).
func (h *host) healthLoop() {
	defer h.wg.Done()
	ticker := time.NewTicker(time.Duration(h.opts.HealthIntervalSeconds) * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-h.stopCh:
			return
		case <-ticker.C:
			h.checkHealth()
		}
	}
}

func (h *host) checkHealth() {
	type probe struct {
		name string
		conn *grpc.ClientConn
	}
	h.mu.RLock()
	probes := make([]probe, 0, len(h.entries))
	for name, e := range h.entries {
		if e.conn != nil {
			probes = append(probes, probe{name: name, conn: e.conn})
		}
	}
	h.mu.RUnlock()

	for _, pr := range probes {
		ctx, cancel := context.WithTimeout(context.Background(), healthRPCTimeout)
		resp, err := pluginv1.NewLifecycleClient(pr.conn).Health(ctx, &pluginv1.HealthRequest{})
		cancel()
		healthy := err == nil && resp.GetOk()

		h.mu.Lock()
		// Re-check identity: a hot-reload swap may have replaced the conn while the
		// probe was in flight; don't clobber the fresh entry with a stale result.
		if e, ok := h.entries[pr.name]; ok && e.conn == pr.conn {
			if healthy {
				e.degraded = false
				e.ready = true
			} else {
				e.degraded = true
				e.ready = false
			}
		}
		h.mu.Unlock()

		if !healthy {
			detail := ""
			if resp != nil {
				detail = resp.GetDetail()
			}
			h.log.Info("plugin health probe failed; marking Degraded",
				"plugin", pr.name, "err", err, "detail", detail)
		}
	}
}

// rawConfig returns the JSON bytes of a CR config block, or nil.
func rawConfig(cfg *runtime.RawExtension) []byte {
	if cfg == nil {
		return nil
	}
	return cfg.Raw
}

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
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
)

// reloadDebounce coalesces the burst of filesystem events an installer emits when it
// swaps a binary (temp write + rename + chmod) into a single reload.
const reloadDebounce = 500 * time.Millisecond

// watchLoop consumes fsnotify events on PluginDir, debounces them, and triggers a
// drain-and-swap reload for each changed binary that backs a loaded plugin.
func (h *host) watchLoop() {
	defer h.wg.Done()

	var timer *time.Timer
	var timerC <-chan time.Time
	changed := make(map[string]struct{})

	for {
		select {
		case <-h.stopCh:
			if timer != nil {
				timer.Stop()
			}
			return

		case ev, ok := <-h.watcher.Events:
			if !ok {
				return
			}
			// Only content-changing ops matter; ignore pure Chmod/Remove churn.
			if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Rename) == 0 {
				continue
			}
			changed[filepath.Base(ev.Name)] = struct{}{}
			if timer == nil {
				timer = time.NewTimer(reloadDebounce)
				timerC = timer.C
			} else {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(reloadDebounce)
			}

		case err, ok := <-h.watcher.Errors:
			if !ok {
				return
			}
			h.log.Error(err, "fsnotify watch error")

		case <-timerC:
			timer = nil
			timerC = nil
			bases := make([]string, 0, len(changed))
			for b := range changed {
				bases = append(bases, b)
				delete(changed, b)
			}
			for _, b := range bases {
				h.reload(b)
			}
		}
	}
}

// reload performs a drain-and-swap for the loaded plugin whose binary basename changed:
// it launches a fresh process, handshakes + Describe/Configure it, atomically swaps the
// conn under the map lock, then drains and terminates the old process. On failure the
// old instance keeps running and is marked Degraded.
func (h *host) reload(binBase string) {
	h.launchMu.Lock()
	defer h.launchMu.Unlock()

	// Find the loaded entry backed by this binary (map is keyed by CR name).
	h.mu.RLock()
	var target *entry
	for _, e := range h.entries {
		if e.binName == binBase {
			target = e
			break
		}
	}
	h.mu.RUnlock()
	if target == nil {
		return // a change to a binary we don't have loaded; nothing to do
	}

	h.log.Info("plugin binary changed; starting drain-and-swap", "plugin", target.name, "bin", binBase)

	spec := launchSpec{
		name:       target.name,
		kind:       target.kind,
		binName:    target.binName,
		binPath:    target.binPath,
		image:      target.image,
		configJSON: target.configJSON,
	}

	newEntry, err := h.launch(context.Background(), spec)
	if err != nil {
		// Keep the previous instance serving; flag it Degraded so the supervisor and
		// (eventually) the controller can surface it.
		h.mu.Lock()
		if cur, ok := h.entries[target.name]; ok {
			cur.degraded = true
		}
		h.mu.Unlock()
		// TODO(m1): surface Degraded on Plugin.status via the controller (host has no CR here).
		h.log.Error(err, "hot-reload failed; keeping previous instance (Degraded)", "plugin", target.name)
		return
	}

	// Atomic swap under the map lock.
	h.mu.Lock()
	old := h.entries[target.name]
	h.entries[target.name] = newEntry
	h.mu.Unlock()

	// Drain + terminate the superseded instance.
	h.drainAndTerminate(context.Background(), old)
	h.log.Info("hot-reload complete", "plugin", target.name, "abi", newEntry.abi, "digest", newEntry.digest)
}

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

// Package controller holds the four controller-runtime reconcilers that drive the
// parallax funnel: Study (§4.1, §7), Trial (§9), Plugin (§5.3/§5.4) and Dataset (§10).
// The operator main and CLI wire them in through the single entrypoint SetupAll.
package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"hash/fnv"
	"strconv"
)

// Finalizers and condition types shared across reconcilers.
const (
	// pluginFinalizer guards a Plugin CR so the host can drain-and-stop the subprocess
	// before the object is removed (DESIGN.md §5.4).
	pluginFinalizer = "parallax.dev/plugin-unload"

	// Condition types stamped on .status.conditions.
	condReady       = "Ready"
	condProgressing = "Progressing"
	condVerified    = "Verified"
	condLoaded      = "Loaded"
)

// shortHash returns a stable 16-hex-char digest of b, used for config hashes and
// spec hashes. It is deterministic so a resumed reconcile keys the same DB rows.
func shortHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:16]
}

// seedFromUID derives a stable, non-negative study seed from an object UID so that
// screening order and strategy sampling are reproducible (DESIGN.md §9).
func seedFromUID(uid string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(uid))
	return int64(h.Sum64() & 0x7fffffffffffffff)
}

// parseInt64 parses a decimal run/trial ID stored as a string on a CR status. It
// returns (0, false) for empty or malformed input so callers can skip DB writes.
func parseInt64(s string) (int64, bool) {
	if s == "" {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

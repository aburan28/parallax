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

package plugin

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
)

// CanonicalHash returns the hex SHA-256 over an assignment map rendered as sorted
// "k=v;" pairs. It is the stable identity of a config point, used for trial
// dedupe/resume (DESIGN.md §14, §15 config_hash).
//
// This lives in the SDK rather than internal/ because it is part of the host↔plugin
// contract: a strategy plugin receives observed and in-flight points as config
// hashes, so it must compute the identical hash to know what it has already
// suggested. A third-party strategy in another module needs this function too.
//
// Assignments are keyed by dimension **name**. A dimension's `path` is where the
// target writes the value and never enters the identity of a config point.
func CanonicalHash(assignments map[string]string) string {
	keys := make([]string, 0, len(assignments))
	for k := range assignments {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(assignments[k])
		b.WriteByte(';')
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

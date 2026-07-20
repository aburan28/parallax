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

// Package migrations embeds the versioned results-DB schema (DESIGN.md §15) for both
// the postgres and sqlite dialects, so store drivers can apply them without a separate
// binary. Files follow the golang-migrate naming convention
// {version}_{name}.{up|down}.sql.
package migrations

import "embed"

// FS holds the dialect subdirectories: postgres/ and sqlite/.
//
//go:embed postgres/*.sql sqlite/*.sql
var FS embed.FS

// Dialect names the embedded subdirectory for a store driver scheme.
func Dialect(scheme string) string {
	switch scheme {
	case "postgres", "postgresql":
		return "postgres"
	case "sqlite", "sqlite3":
		return "sqlite"
	default:
		return ""
	}
}

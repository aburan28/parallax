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

// Package store is the results database abstraction (DESIGN.md §15). PostgreSQL is
// the system of record; SQLite backs --local mode behind the same interface. Concrete
// drivers live in the postgres/ and sqlite/ subpackages and self-register via Register.
package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Store is the results database. Writes are transactional and idempotent keyed on
// (run_id, config_hash, rep) so a crashed controller resumes cleanly (DESIGN.md §15).
type Store interface {
	// Migrate applies pending schema migrations (only when db.autoMigrate is set).
	Migrate(ctx context.Context) error
	// Ping verifies connectivity.
	Ping(ctx context.Context) error
	// Close releases the underlying connection pool.
	Close() error

	// Study / run lifecycle.
	UpsertStudy(ctx context.Context, s *StudyRecord) (int64, error)
	CreateRun(ctx context.Context, r *RunRecord) (int64, error)
	UpdateRunPhase(ctx context.Context, runID int64, phase string) error

	// Trials and their SLI evidence.
	UpsertTrial(ctx context.Context, t *TrialRecord) (int64, error)
	InsertSLIValues(ctx context.Context, values []SLIValueRecord) error
	InsertObservation(ctx context.Context, o *ObservationRecord) error
	InsertArtifact(ctx context.Context, a *ArtifactRecord) error

	// Decisions, validation, promotion.
	InsertDecision(ctx context.Context, d *DecisionRecord) (int64, error)
	InsertValidation(ctx context.Context, v *ValidationRecord) error
	InsertScenario(ctx context.Context, s *ScenarioRecord) error
	InsertPromotion(ctx context.Context, p *PromotionRecord) error

	// Datasets and the plugin audit trail.
	UpsertDataset(ctx context.Context, d *DatasetRecord) error
	RecordPluginAudit(ctx context.Context, a *PluginAuditRecord) error

	// Read paths for `parallax select` / `parallax report` (offline replay).
	GetRun(ctx context.Context, runID int64) (*RunRecord, error)
	ListTrialsForRun(ctx context.Context, runID int64) ([]TrialRecord, error)
	ListSLIValuesForRun(ctx context.Context, runID int64) ([]SLIValueRecord, error)
}

// OpenFunc constructs a Store from a driver-specific DSN.
type OpenFunc func(ctx context.Context, dsn string) (Store, error)

var (
	driversMu sync.RWMutex
	drivers   = map[string]OpenFunc{}
)

// Register makes a store driver available under a DSN scheme (e.g. "sqlite",
// "postgres"). Drivers call this from an init() function; wire them in with a blank
// import: `_ "github.com/aburan28/parallax/internal/store/sqlite"`.
func Register(scheme string, f OpenFunc) {
	driversMu.Lock()
	defer driversMu.Unlock()
	if f == nil {
		panic("store: Register with nil OpenFunc for scheme " + scheme)
	}
	if _, dup := drivers[scheme]; dup {
		panic("store: duplicate driver registration for scheme " + scheme)
	}
	drivers[scheme] = f
}

// Open dispatches on the DSN scheme to a registered driver. Examples:
//
//	sqlite:///var/lib/parallax/local.db
//	postgres://user:pass@host:5432/parallax?sslmode=require
func Open(ctx context.Context, dsn string) (Store, error) {
	scheme, _, ok := strings.Cut(dsn, ":")
	if !ok || scheme == "" {
		return nil, fmt.Errorf("store: malformed DSN %q (want scheme:...)", dsn)
	}
	// Normalize postgresql:// → postgres.
	if scheme == "postgresql" {
		scheme = "postgres"
	}
	driversMu.RLock()
	f, known := drivers[scheme]
	driversMu.RUnlock()
	if !known {
		return nil, fmt.Errorf("store: no driver registered for scheme %q (have: %s)", scheme, strings.Join(registered(), ", "))
	}
	return f(ctx, dsn)
}

func registered() []string {
	driversMu.RLock()
	defer driversMu.RUnlock()
	out := make([]string, 0, len(drivers))
	for k := range drivers {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

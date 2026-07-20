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

// Package postgres is the PostgreSQL driver for the results store (DESIGN.md §15).
// Postgres is the system of record in cluster mode. The pool is provided by
// github.com/jackc/pgx/v5/pgxpool (pure Go, CGO_ENABLED=0). It self-registers
// under the "postgres" and "postgresql" DSN schemes.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/aburan28/parallax/internal/store"
	"github.com/aburan28/parallax/migrations"
)

func init() {
	store.Register("postgres", Open)
	store.Register("postgresql", Open)
}

// DB is the concrete store.Store backed by a pgx connection pool.
type DB struct {
	pool *pgxpool.Pool
}

var _ store.Store = (*DB)(nil)

// Open constructs a Postgres-backed store from a libpq-style DSN, e.g.
//
//	postgres://user:pass@host:5432/parallax?sslmode=require
//	postgresql://host/parallax
//
// The DSN is passed through to pgx unchanged.
func Open(ctx context.Context, dsn string) (store.Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse DSN: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: open pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return &DB{pool: pool}, nil
}

// Migrate applies every embedded postgres *.up.sql migration in lexical order.
// The DDL is idempotent (CREATE TABLE/INDEX IF NOT EXISTS); each file is sent as
// a single simple-protocol batch (no bind parameters).
//
// TODO(m1): track applied versions in a schema_migrations table and support the
// paired *.down.sql rollbacks instead of relying on IF NOT EXISTS idempotency.
func (d *DB) Migrate(ctx context.Context) error {
	const dir = "postgres"
	entries, err := migrations.FS.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("postgres: read embedded migrations: %w", err)
	}
	var ups []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		ups = append(ups, name)
	}
	sort.Strings(ups)
	if len(ups) == 0 {
		return fmt.Errorf("postgres: no *.up.sql migrations embedded under %q", dir)
	}
	for _, name := range ups {
		b, err := migrations.FS.ReadFile(dir + "/" + name)
		if err != nil {
			return fmt.Errorf("postgres: read migration %s: %w", name, err)
		}
		// No args → pgx uses the simple protocol, which permits multiple
		// statements per Exec.
		if _, err := d.pool.Exec(ctx, string(b)); err != nil {
			return fmt.Errorf("postgres: apply migration %s: %w", name, err)
		}
	}
	return nil
}

// Ping verifies connectivity.
func (d *DB) Ping(ctx context.Context) error {
	if err := d.pool.Ping(ctx); err != nil {
		return fmt.Errorf("postgres: ping: %w", err)
	}
	return nil
}

// Close releases the underlying pool.
func (d *DB) Close() error {
	d.pool.Close()
	return nil
}

// UpsertStudy inserts or updates a study keyed on (namespace, name, spec_hash)
// and returns its id.
func (d *DB) UpsertStudy(ctx context.Context, s *store.StudyRecord) (int64, error) {
	if s == nil {
		return 0, errors.New("postgres: UpsertStudy: nil record")
	}
	const q = `
INSERT INTO studies (name, namespace, spec, spec_hash)
VALUES ($1, $2, $3, $4)
ON CONFLICT (namespace, name, spec_hash) DO UPDATE SET spec = excluded.spec
RETURNING id`
	var id int64
	if err := d.pool.QueryRow(ctx, q, s.Name, s.Namespace, jsonReq(s.Spec), s.SpecHash).Scan(&id); err != nil {
		return 0, fmt.Errorf("postgres: upsert study %s/%s: %w", s.Namespace, s.Name, err)
	}
	return id, nil
}

// CreateRun inserts a run and returns its id.
func (d *DB) CreateRun(ctx context.Context, r *store.RunRecord) (int64, error) {
	if r == nil {
		return 0, errors.New("postgres: CreateRun: nil record")
	}
	phase := r.Phase
	if phase == "" {
		phase = "Pending"
	}
	const q = `
INSERT INTO runs (study_id, seed, fingerprint, phase, started_at, completed_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id`
	var id int64
	if err := d.pool.QueryRow(ctx, q, r.StudyID, r.Seed, jsonNull(r.Fingerprint), phase,
		r.StartedAt, r.CompletedAt).Scan(&id); err != nil {
		return 0, fmt.Errorf("postgres: create run for study %d: %w", r.StudyID, err)
	}
	return id, nil
}

// UpdateRunPhase sets the phase of a run.
func (d *DB) UpdateRunPhase(ctx context.Context, runID int64, phase string) error {
	const q = `UPDATE runs SET phase = $1 WHERE id = $2`
	tag, err := d.pool.Exec(ctx, q, phase, runID)
	if err != nil {
		return fmt.Errorf("postgres: update run %d phase: %w", runID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("postgres: update run %d phase: run not found", runID)
	}
	return nil
}

// UpsertTrial inserts or updates a trial keyed on (run_id, config_hash, rep) and
// returns its id.
func (d *DB) UpsertTrial(ctx context.Context, t *store.TrialRecord) (int64, error) {
	if t == nil {
		return 0, errors.New("postgres: UpsertTrial: nil record")
	}
	const q = `
INSERT INTO trials (run_id, config_hash, config, workload, fidelity, rep, mode, phase, validity, timeline)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (run_id, config_hash, rep) DO UPDATE SET
    config   = excluded.config,
    workload = excluded.workload,
    fidelity = excluded.fidelity,
    mode     = excluded.mode,
    phase    = excluded.phase,
    validity = excluded.validity,
    timeline = excluded.timeline
RETURNING id`
	var id int64
	if err := d.pool.QueryRow(ctx, q,
		t.RunID, t.ConfigHash, jsonNull(t.Config), nullStr(t.Workload), jsonNull(t.Fidelity),
		t.Rep, nullStr(t.Mode), nullStr(t.Phase), nullStr(t.Validity), jsonNull(t.Timeline)).Scan(&id); err != nil {
		return 0, fmt.Errorf("postgres: upsert trial run=%d hash=%s rep=%d: %w", t.RunID, t.ConfigHash, t.Rep, err)
	}
	return id, nil
}

// InsertSLIValues bulk-inserts SLI facts in a single transaction.
func (d *DB) InsertSLIValues(ctx context.Context, values []store.SLIValueRecord) error {
	if len(values) == 0 {
		return nil
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: insert sli values: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	const q = `
INSERT INTO sli_values (trial_id, name, provider, class, value, query, evaluated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)`
	for i := range values {
		v := &values[i]
		at := v.EvaluatedAt
		if at.IsZero() {
			at = time.Now().UTC()
		}
		if _, err := tx.Exec(ctx, q, v.TrialID, v.Name, nullStr(v.Provider), nullStr(v.Class),
			v.Value, nullStr(v.Query), at); err != nil {
			return fmt.Errorf("postgres: insert sli value %q for trial %d: %w", v.Name, v.TrialID, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: insert sli values: commit: %w", err)
	}
	return nil
}

// InsertObservation appends a row to the search log.
func (d *DB) InsertObservation(ctx context.Context, o *store.ObservationRecord) error {
	if o == nil {
		return errors.New("postgres: InsertObservation: nil record")
	}
	const q = `
INSERT INTO observations (run_id, seq, config_hash, fidelity, objective, budget_counters)
VALUES ($1, $2, $3, $4, $5, $6)`
	if _, err := d.pool.Exec(ctx, q, o.RunID, o.Seq, o.ConfigHash,
		jsonNull(o.Fidelity), o.Objective, jsonNull(o.BudgetCounters)); err != nil {
		return fmt.Errorf("postgres: insert observation run=%d seq=%d: %w", o.RunID, o.Seq, err)
	}
	return nil
}

// InsertArtifact records an artifact reference for a trial.
func (d *DB) InsertArtifact(ctx context.Context, a *store.ArtifactRecord) error {
	if a == nil {
		return errors.New("postgres: InsertArtifact: nil record")
	}
	const q = `INSERT INTO artifacts (trial_id, kind, uri, digest, bytes) VALUES ($1, $2, $3, $4, $5)`
	if _, err := d.pool.Exec(ctx, q, a.TrialID, a.Kind, a.URI, nullStr(a.Digest), a.Bytes); err != nil {
		return fmt.Errorf("postgres: insert artifact %s for trial %d: %w", a.Kind, a.TrialID, err)
	}
	return nil
}

// InsertDecision records a search/analysis decision and returns its id.
func (d *DB) InsertDecision(ctx context.Context, dec *store.DecisionRecord) (int64, error) {
	if dec == nil {
		return 0, errors.New("postgres: InsertDecision: nil record")
	}
	const q = `INSERT INTO decisions (run_id, stage, record) VALUES ($1, $2, $3) RETURNING id`
	var id int64
	if err := d.pool.QueryRow(ctx, q, dec.RunID, dec.Stage, jsonReq(dec.Record)).Scan(&id); err != nil {
		return 0, fmt.Errorf("postgres: insert decision run=%d stage=%s: %w", dec.RunID, dec.Stage, err)
	}
	return id, nil
}

// InsertValidation records a validation stats blob for a candidate.
func (d *DB) InsertValidation(ctx context.Context, v *store.ValidationRecord) error {
	if v == nil {
		return errors.New("postgres: InsertValidation: nil record")
	}
	const q = `INSERT INTO validations (run_id, candidate_hash, stats) VALUES ($1, $2, $3)`
	if _, err := d.pool.Exec(ctx, q, v.RunID, v.CandidateHash, jsonReq(v.Stats)); err != nil {
		return fmt.Errorf("postgres: insert validation run=%d candidate=%s: %w", v.RunID, v.CandidateHash, err)
	}
	return nil
}

// InsertScenario records a scenario verdict for a candidate.
func (d *DB) InsertScenario(ctx context.Context, s *store.ScenarioRecord) error {
	if s == nil {
		return errors.New("postgres: InsertScenario: nil record")
	}
	const q = `INSERT INTO scenarios (run_id, candidate_hash, scenario, verdict) VALUES ($1, $2, $3, $4)`
	if _, err := d.pool.Exec(ctx, q, s.RunID, s.CandidateHash, s.Scenario, jsonReq(s.Verdict)); err != nil {
		return fmt.Errorf("postgres: insert scenario %q run=%d candidate=%s: %w", s.Scenario, s.RunID, s.CandidateHash, err)
	}
	return nil
}

// InsertPromotion records a promotion approval.
func (d *DB) InsertPromotion(ctx context.Context, p *store.PromotionRecord) error {
	if p == nil {
		return errors.New("postgres: InsertPromotion: nil record")
	}
	at := p.ApprovedAt
	if at.IsZero() {
		at = time.Now().UTC()
	}
	const q = `INSERT INTO promotions (run_id, artifact_ref, approver, approved_at) VALUES ($1, $2, $3, $4)`
	if _, err := d.pool.Exec(ctx, q, p.RunID, nullStr(p.ArtifactRef), nullStr(p.Approver), at); err != nil {
		return fmt.Errorf("postgres: insert promotion run=%d: %w", p.RunID, err)
	}
	return nil
}

// UpsertDataset inserts or updates a dataset keyed on its unique name.
func (d *DB) UpsertDataset(ctx context.Context, ds *store.DatasetRecord) error {
	if ds == nil {
		return errors.New("postgres: UpsertDataset: nil record")
	}
	const q = `
INSERT INTO datasets (name, manifest, id_digest, verified_at)
VALUES ($1, $2, $3, $4)
ON CONFLICT (name) DO UPDATE SET
    manifest    = excluded.manifest,
    id_digest   = excluded.id_digest,
    verified_at = excluded.verified_at`
	if _, err := d.pool.Exec(ctx, q, ds.Name, jsonNull(ds.Manifest), nullStr(ds.IDDigest), ds.VerifiedAt); err != nil {
		return fmt.Errorf("postgres: upsert dataset %q: %w", ds.Name, err)
	}
	return nil
}

// RecordPluginAudit appends a row to the plugin audit trail.
func (d *DB) RecordPluginAudit(ctx context.Context, a *store.PluginAuditRecord) error {
	if a == nil {
		return errors.New("postgres: RecordPluginAudit: nil record")
	}
	at := a.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	const q = `INSERT INTO plugin_audit (plugin, kind, digest, action, actor, at) VALUES ($1, $2, $3, $4, $5, $6)`
	if _, err := d.pool.Exec(ctx, q, a.Plugin, nullStr(a.Kind), nullStr(a.Digest), a.Action, nullStr(a.Actor), at); err != nil {
		return fmt.Errorf("postgres: record plugin audit %s/%s: %w", a.Plugin, a.Action, err)
	}
	return nil
}

// GetRun loads a single run by id.
func (d *DB) GetRun(ctx context.Context, runID int64) (*store.RunRecord, error) {
	const q = `SELECT id, study_id, seed, fingerprint, phase, started_at, completed_at FROM runs WHERE id = $1`
	var (
		r  store.RunRecord
		fp []byte
	)
	err := d.pool.QueryRow(ctx, q, runID).Scan(&r.ID, &r.StudyID, &r.Seed, &fp, &r.Phase, &r.StartedAt, &r.CompletedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("postgres: run %d not found: %w", runID, err)
		}
		return nil, fmt.Errorf("postgres: get run %d: %w", runID, err)
	}
	r.Fingerprint = rawOrNil(fp)
	return &r, nil
}

// ListTrialsForRun returns all trials for a run ordered by id.
func (d *DB) ListTrialsForRun(ctx context.Context, runID int64) ([]store.TrialRecord, error) {
	const q = `
SELECT id, run_id, config_hash, config, workload, fidelity, rep, mode, phase, validity, timeline, created_at
FROM trials WHERE run_id = $1 ORDER BY id`
	rows, err := d.pool.Query(ctx, q, runID)
	if err != nil {
		return nil, fmt.Errorf("postgres: list trials for run %d: %w", runID, err)
	}
	defer rows.Close()

	var out []store.TrialRecord
	for rows.Next() {
		var (
			t        store.TrialRecord
			config   []byte
			fidelity []byte
			timeline []byte
			workload *string
			mode     *string
			phase    *string
			validity *string
		)
		if err := rows.Scan(&t.ID, &t.RunID, &t.ConfigHash, &config, &workload, &fidelity,
			&t.Rep, &mode, &phase, &validity, &timeline, &t.CreatedAt); err != nil {
			return nil, fmt.Errorf("postgres: scan trial for run %d: %w", runID, err)
		}
		t.Config = rawOrNil(config)
		t.Fidelity = rawOrNil(fidelity)
		t.Timeline = rawOrNil(timeline)
		t.Workload = deref(workload)
		t.Mode = deref(mode)
		t.Phase = deref(phase)
		t.Validity = deref(validity)
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: iterate trials for run %d: %w", runID, err)
	}
	return out, nil
}

// ListSLIValuesForRun returns every SLI value across all trials of a run.
func (d *DB) ListSLIValuesForRun(ctx context.Context, runID int64) ([]store.SLIValueRecord, error) {
	const q = `
SELECT s.id, s.trial_id, s.name, s.provider, s.class, s.value, s.query, s.evaluated_at
FROM sli_values s
JOIN trials t ON t.id = s.trial_id
WHERE t.run_id = $1
ORDER BY s.id`
	rows, err := d.pool.Query(ctx, q, runID)
	if err != nil {
		return nil, fmt.Errorf("postgres: list sli values for run %d: %w", runID, err)
	}
	defer rows.Close()

	var out []store.SLIValueRecord
	for rows.Next() {
		var (
			v        store.SLIValueRecord
			provider *string
			class    *string
			query    *string
			value    *float64
		)
		if err := rows.Scan(&v.ID, &v.TrialID, &v.Name, &provider, &class, &value, &query, &v.EvaluatedAt); err != nil {
			return nil, fmt.Errorf("postgres: scan sli value for run %d: %w", runID, err)
		}
		v.Provider = deref(provider)
		v.Class = deref(class)
		v.Query = deref(query)
		if value != nil {
			v.Value = *value
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("postgres: iterate sli values for run %d: %w", runID, err)
	}
	return out, nil
}

// --- binding / scanning helpers -------------------------------------------------

// jsonNull binds a nullable jsonb column: empty → SQL NULL, else raw bytes.
func jsonNull(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return []byte(raw)
}

// jsonReq binds a NOT NULL jsonb column: empty → the JSON literal "null".
func jsonReq(raw json.RawMessage) any {
	if len(raw) == 0 {
		return []byte("null")
	}
	return []byte(raw)
}

// nullStr binds a nullable text column: "" → SQL NULL, else the string.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// deref returns the pointed-to string or "" for a nil pointer (NULL column).
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// rawOrNil converts scanned bytes to json.RawMessage, preserving nil for NULL.
func rawOrNil(b []byte) json.RawMessage {
	if b == nil {
		return nil
	}
	return json.RawMessage(b)
}

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

// Package sqlite is the pure-Go (CGO_ENABLED=0) SQLite driver for the results
// store (DESIGN.md §15). It backs `parallax --local` mode behind the same
// store.Store interface as the postgres driver. The concrete database engine is
// modernc.org/sqlite (registered with database/sql under the name "sqlite").
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	// modernc.org/sqlite registers the pure-Go "sqlite" database/sql driver.
	_ "modernc.org/sqlite"

	"github.com/aburan28/parallax/internal/store"
	"github.com/aburan28/parallax/migrations"
)

// driverName is the database/sql driver registered by modernc.org/sqlite.
const driverName = "sqlite"

func init() {
	store.Register("sqlite", Open)
	store.Register("sqlite3", Open)
}

// DB is the concrete store.Store backed by a database/sql pool over modernc SQLite.
type DB struct {
	db  *sql.DB
	mem bool
}

var _ store.Store = (*DB)(nil)

// Open constructs a SQLite-backed store from a DSN. Accepted forms:
//
//	sqlite:///var/lib/parallax/local.db   → file /var/lib/parallax/local.db
//	sqlite3:///tmp/x.db                   → file /tmp/x.db
//	sqlite::memory:                       → shared in-memory database
//	:memory:                              → shared in-memory database
//	file:local.db?cache=shared            → passed through to the driver
//
// Foreign-key enforcement is enabled on every pooled connection.
func Open(ctx context.Context, dsn string) (store.Store, error) {
	path, mem, err := parseDSN(dsn)
	if err != nil {
		return nil, err
	}

	connStr := path
	if !mem {
		// Enable foreign keys + a busy timeout on *every* pooled connection by
		// carrying them in the DSN (a one-shot PRAGMA would only touch one conn).
		sep := "?"
		if strings.Contains(connStr, "?") {
			sep = "&"
		}
		connStr += sep + "_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	}

	sqlDB, err := sql.Open(driverName, connStr)
	if err != nil {
		return nil, fmt.Errorf("sqlite: open %q: %w", dsn, err)
	}
	if mem {
		// A ":memory:" database is per-connection; pin the pool to a single
		// connection so every statement sees the same database.
		sqlDB.SetMaxOpenConns(1)
	}

	d := &DB{db: sqlDB, mem: mem}

	// Belt-and-suspenders: enable FKs on the connection we ping with.
	if _, err := sqlDB.ExecContext(ctx, "PRAGMA foreign_keys=ON;"); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("sqlite: enable foreign_keys: %w", err)
	}
	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("sqlite: ping %q: %w", dsn, err)
	}
	return d, nil
}

// parseDSN strips a sqlite scheme to a filesystem path and detects in-memory forms.
func parseDSN(dsn string) (path string, mem bool, err error) {
	s := strings.TrimSpace(dsn)
	if s == "" {
		return "", false, errors.New("sqlite: empty DSN")
	}
	// Strip a recognised scheme prefix. "sqlite://" must be tried before
	// "sqlite:" so that sqlite:///abs → /abs (three slashes collapse to one).
	for _, pfx := range []string{"sqlite://", "sqlite3://", "sqlite:", "sqlite3:"} {
		if strings.HasPrefix(s, pfx) {
			s = strings.TrimPrefix(s, pfx)
			break
		}
	}
	if s == "" {
		return "", false, fmt.Errorf("sqlite: DSN %q has no path", dsn)
	}
	// In-memory forms: ":memory:", "/:memory:" (from sqlite:///:memory:), or
	// a bare "file::memory:".
	switch {
	case s == ":memory:", s == "/:memory:":
		return ":memory:", true, nil
	case strings.HasPrefix(s, "file::memory:"), strings.Contains(s, "mode=memory"):
		return s, true, nil
	}
	return s, false, nil
}

// Migrate applies every embedded sqlite *.up.sql migration in lexical order. The
// DDL is idempotent (CREATE TABLE/INDEX IF NOT EXISTS).
//
// TODO(m1): track applied versions in a schema_migrations table and support the
// paired *.down.sql rollbacks instead of relying on IF NOT EXISTS idempotency.
func (d *DB) Migrate(ctx context.Context) error {
	const dir = "sqlite"
	entries, err := migrations.FS.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("sqlite: read embedded migrations: %w", err)
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
		return fmt.Errorf("sqlite: no *.up.sql migrations embedded under %q", dir)
	}
	for _, name := range ups {
		b, err := migrations.FS.ReadFile(dir + "/" + name)
		if err != nil {
			return fmt.Errorf("sqlite: read migration %s: %w", name, err)
		}
		if _, err := d.db.ExecContext(ctx, string(b)); err != nil {
			return fmt.Errorf("sqlite: apply migration %s: %w", name, err)
		}
	}
	return nil
}

// Ping verifies connectivity.
func (d *DB) Ping(ctx context.Context) error {
	if err := d.db.PingContext(ctx); err != nil {
		return fmt.Errorf("sqlite: ping: %w", err)
	}
	return nil
}

// Close releases the underlying pool.
func (d *DB) Close() error {
	if err := d.db.Close(); err != nil {
		return fmt.Errorf("sqlite: close: %w", err)
	}
	return nil
}

// UpsertStudy inserts or updates a study keyed on (namespace, name, spec_hash)
// and returns its id.
func (d *DB) UpsertStudy(ctx context.Context, s *store.StudyRecord) (int64, error) {
	if s == nil {
		return 0, errors.New("sqlite: UpsertStudy: nil record")
	}
	const q = `
INSERT INTO studies (name, namespace, spec, spec_hash)
VALUES (?, ?, ?, ?)
ON CONFLICT (namespace, name, spec_hash) DO UPDATE SET spec = excluded.spec`
	if _, err := d.db.ExecContext(ctx, q, s.Name, s.Namespace, jsonReq(s.Spec), s.SpecHash); err != nil {
		return 0, fmt.Errorf("sqlite: upsert study %s/%s: %w", s.Namespace, s.Name, err)
	}
	// SQLite does not update last_insert_rowid() on the ON CONFLICT DO UPDATE
	// path, so resolve the id authoritatively via the unique key.
	var id int64
	const sel = `SELECT id FROM studies WHERE namespace = ? AND name = ? AND spec_hash = ?`
	if err := d.db.QueryRowContext(ctx, sel, s.Namespace, s.Name, s.SpecHash).Scan(&id); err != nil {
		return 0, fmt.Errorf("sqlite: resolve study id %s/%s: %w", s.Namespace, s.Name, err)
	}
	return id, nil
}

// CreateRun inserts a run and returns its id via LastInsertId.
func (d *DB) CreateRun(ctx context.Context, r *store.RunRecord) (int64, error) {
	if r == nil {
		return 0, errors.New("sqlite: CreateRun: nil record")
	}
	phase := r.Phase
	if phase == "" {
		phase = "Pending"
	}
	const q = `
INSERT INTO runs (study_id, seed, fingerprint, phase, started_at, completed_at)
VALUES (?, ?, ?, ?, ?, ?)`
	res, err := d.db.ExecContext(ctx, q, r.StudyID, r.Seed, jsonNull(r.Fingerprint), phase,
		nullableTime(r.StartedAt), nullableTime(r.CompletedAt))
	if err != nil {
		return 0, fmt.Errorf("sqlite: create run for study %d: %w", r.StudyID, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("sqlite: create run last insert id: %w", err)
	}
	return id, nil
}

// UpdateRunPhase sets the phase of a run.
func (d *DB) UpdateRunPhase(ctx context.Context, runID int64, phase string) error {
	const q = `UPDATE runs SET phase = ? WHERE id = ?`
	res, err := d.db.ExecContext(ctx, q, phase, runID)
	if err != nil {
		return fmt.Errorf("sqlite: update run %d phase: %w", runID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("sqlite: update run %d phase rows affected: %w", runID, err)
	}
	if n == 0 {
		return fmt.Errorf("sqlite: update run %d phase: run not found", runID)
	}
	return nil
}

// UpsertTrial inserts or updates a trial keyed on (run_id, config_hash, rep) and
// returns its id.
func (d *DB) UpsertTrial(ctx context.Context, t *store.TrialRecord) (int64, error) {
	if t == nil {
		return 0, errors.New("sqlite: UpsertTrial: nil record")
	}
	const q = `
INSERT INTO trials (run_id, config_hash, config, workload, fidelity, rep, mode, phase, validity, timeline)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (run_id, config_hash, rep) DO UPDATE SET
    config   = excluded.config,
    workload = excluded.workload,
    fidelity = excluded.fidelity,
    mode     = excluded.mode,
    phase    = excluded.phase,
    validity = excluded.validity,
    timeline = excluded.timeline`
	if _, err := d.db.ExecContext(ctx, q,
		t.RunID, t.ConfigHash, jsonNull(t.Config), t.Workload, jsonNull(t.Fidelity),
		t.Rep, t.Mode, t.Phase, t.Validity, jsonNull(t.Timeline)); err != nil {
		return 0, fmt.Errorf("sqlite: upsert trial run=%d hash=%s rep=%d: %w", t.RunID, t.ConfigHash, t.Rep, err)
	}
	// Resolve id via the unique key (last_insert_rowid is unreliable on upsert).
	var id int64
	const sel = `SELECT id FROM trials WHERE run_id = ? AND config_hash = ? AND rep = ?`
	if err := d.db.QueryRowContext(ctx, sel, t.RunID, t.ConfigHash, t.Rep).Scan(&id); err != nil {
		return 0, fmt.Errorf("sqlite: resolve trial id run=%d hash=%s rep=%d: %w", t.RunID, t.ConfigHash, t.Rep, err)
	}
	return id, nil
}

// InsertSLIValues bulk-inserts SLI facts in a single transaction.
func (d *DB) InsertSLIValues(ctx context.Context, values []store.SLIValueRecord) error {
	if len(values) == 0 {
		return nil
	}
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: insert sli values: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const q = `
INSERT INTO sli_values (trial_id, name, provider, class, value, query, evaluated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)`
	stmt, err := tx.PrepareContext(ctx, q)
	if err != nil {
		return fmt.Errorf("sqlite: insert sli values: prepare: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	for i := range values {
		v := &values[i]
		at := v.EvaluatedAt
		if at.IsZero() {
			at = time.Now().UTC()
		}
		if _, err := stmt.ExecContext(ctx, v.TrialID, v.Name, v.Provider, v.Class, v.Value, v.Query, at); err != nil {
			return fmt.Errorf("sqlite: insert sli value %q for trial %d: %w", v.Name, v.TrialID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: insert sli values: commit: %w", err)
	}
	return nil
}

// InsertObservation appends a row to the search log.
func (d *DB) InsertObservation(ctx context.Context, o *store.ObservationRecord) error {
	if o == nil {
		return errors.New("sqlite: InsertObservation: nil record")
	}
	const q = `
INSERT INTO observations (run_id, seq, config_hash, fidelity, objective, budget_counters)
VALUES (?, ?, ?, ?, ?, ?)`
	if _, err := d.db.ExecContext(ctx, q, o.RunID, o.Seq, o.ConfigHash,
		jsonNull(o.Fidelity), o.Objective, jsonNull(o.BudgetCounters)); err != nil {
		return fmt.Errorf("sqlite: insert observation run=%d seq=%d: %w", o.RunID, o.Seq, err)
	}
	return nil
}

// InsertArtifact records an artifact reference for a trial.
func (d *DB) InsertArtifact(ctx context.Context, a *store.ArtifactRecord) error {
	if a == nil {
		return errors.New("sqlite: InsertArtifact: nil record")
	}
	const q = `INSERT INTO artifacts (trial_id, kind, uri, digest, bytes) VALUES (?, ?, ?, ?, ?)`
	if _, err := d.db.ExecContext(ctx, q, a.TrialID, a.Kind, a.URI, a.Digest, a.Bytes); err != nil {
		return fmt.Errorf("sqlite: insert artifact %s for trial %d: %w", a.Kind, a.TrialID, err)
	}
	return nil
}

// InsertDecision records a search/analysis decision and returns its id.
func (d *DB) InsertDecision(ctx context.Context, dec *store.DecisionRecord) (int64, error) {
	if dec == nil {
		return 0, errors.New("sqlite: InsertDecision: nil record")
	}
	const q = `INSERT INTO decisions (run_id, stage, record) VALUES (?, ?, ?)`
	res, err := d.db.ExecContext(ctx, q, dec.RunID, dec.Stage, jsonReq(dec.Record))
	if err != nil {
		return 0, fmt.Errorf("sqlite: insert decision run=%d stage=%s: %w", dec.RunID, dec.Stage, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("sqlite: insert decision last insert id: %w", err)
	}
	return id, nil
}

// InsertValidation records a validation stats blob for a candidate.
func (d *DB) InsertValidation(ctx context.Context, v *store.ValidationRecord) error {
	if v == nil {
		return errors.New("sqlite: InsertValidation: nil record")
	}
	const q = `INSERT INTO validations (run_id, candidate_hash, stats) VALUES (?, ?, ?)`
	if _, err := d.db.ExecContext(ctx, q, v.RunID, v.CandidateHash, jsonReq(v.Stats)); err != nil {
		return fmt.Errorf("sqlite: insert validation run=%d candidate=%s: %w", v.RunID, v.CandidateHash, err)
	}
	return nil
}

// InsertScenario records a scenario verdict for a candidate.
func (d *DB) InsertScenario(ctx context.Context, s *store.ScenarioRecord) error {
	if s == nil {
		return errors.New("sqlite: InsertScenario: nil record")
	}
	const q = `INSERT INTO scenarios (run_id, candidate_hash, scenario, verdict) VALUES (?, ?, ?, ?)`
	if _, err := d.db.ExecContext(ctx, q, s.RunID, s.CandidateHash, s.Scenario, jsonReq(s.Verdict)); err != nil {
		return fmt.Errorf("sqlite: insert scenario %q run=%d candidate=%s: %w", s.Scenario, s.RunID, s.CandidateHash, err)
	}
	return nil
}

// InsertPromotion records a promotion approval.
func (d *DB) InsertPromotion(ctx context.Context, p *store.PromotionRecord) error {
	if p == nil {
		return errors.New("sqlite: InsertPromotion: nil record")
	}
	at := p.ApprovedAt
	if at.IsZero() {
		at = time.Now().UTC()
	}
	const q = `INSERT INTO promotions (run_id, artifact_ref, approver, approved_at) VALUES (?, ?, ?, ?)`
	if _, err := d.db.ExecContext(ctx, q, p.RunID, p.ArtifactRef, p.Approver, at); err != nil {
		return fmt.Errorf("sqlite: insert promotion run=%d: %w", p.RunID, err)
	}
	return nil
}

// UpsertDataset inserts or updates a dataset keyed on its unique name.
func (d *DB) UpsertDataset(ctx context.Context, ds *store.DatasetRecord) error {
	if ds == nil {
		return errors.New("sqlite: UpsertDataset: nil record")
	}
	const q = `
INSERT INTO datasets (name, manifest, id_digest, verified_at)
VALUES (?, ?, ?, ?)
ON CONFLICT (name) DO UPDATE SET
    manifest    = excluded.manifest,
    id_digest   = excluded.id_digest,
    verified_at = excluded.verified_at`
	if _, err := d.db.ExecContext(ctx, q, ds.Name, jsonNull(ds.Manifest), ds.IDDigest, nullableTime(ds.VerifiedAt)); err != nil {
		return fmt.Errorf("sqlite: upsert dataset %q: %w", ds.Name, err)
	}
	return nil
}

// RecordPluginAudit appends a row to the plugin audit trail.
func (d *DB) RecordPluginAudit(ctx context.Context, a *store.PluginAuditRecord) error {
	if a == nil {
		return errors.New("sqlite: RecordPluginAudit: nil record")
	}
	at := a.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	const q = `INSERT INTO plugin_audit (plugin, kind, digest, action, actor, at) VALUES (?, ?, ?, ?, ?, ?)`
	if _, err := d.db.ExecContext(ctx, q, a.Plugin, a.Kind, a.Digest, a.Action, a.Actor, at); err != nil {
		return fmt.Errorf("sqlite: record plugin audit %s/%s: %w", a.Plugin, a.Action, err)
	}
	return nil
}

// GetStudy fetches a study record by id (used by offline `parallax select`).
func (d *DB) GetStudy(ctx context.Context, studyID int64) (*store.StudyRecord, error) {
	const q = `SELECT id, name, namespace, spec, spec_hash, created_at FROM studies WHERE id = ?`
	var (
		s       store.StudyRecord
		spec    []byte
		created nullTime
	)
	err := d.db.QueryRowContext(ctx, q, studyID).Scan(&s.ID, &s.Name, &s.Namespace, &spec, &s.SpecHash, &created)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("sqlite: study %d not found: %w", studyID, err)
		}
		return nil, fmt.Errorf("sqlite: get study %d: %w", studyID, err)
	}
	s.Spec = rawOrNil(spec)
	if t := created.ptr(); t != nil {
		s.CreatedAt = *t
	}
	return &s, nil
}

// GetRun loads a single run by id.
func (d *DB) GetRun(ctx context.Context, runID int64) (*store.RunRecord, error) {
	const q = `SELECT id, study_id, seed, fingerprint, phase, started_at, completed_at FROM runs WHERE id = ?`
	var (
		r         store.RunRecord
		fp        []byte
		started   nullTime
		completed nullTime
	)
	err := d.db.QueryRowContext(ctx, q, runID).Scan(&r.ID, &r.StudyID, &r.Seed, &fp, &r.Phase, &started, &completed)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("sqlite: run %d not found: %w", runID, err)
		}
		return nil, fmt.Errorf("sqlite: get run %d: %w", runID, err)
	}
	r.Fingerprint = rawOrNil(fp)
	r.StartedAt = started.ptr()
	r.CompletedAt = completed.ptr()
	return &r, nil
}

// ListTrialsForRun returns all trials for a run ordered by id.
func (d *DB) ListTrialsForRun(ctx context.Context, runID int64) ([]store.TrialRecord, error) {
	const q = `
SELECT id, run_id, config_hash, config, workload, fidelity, rep, mode, phase, validity, timeline, created_at
FROM trials WHERE run_id = ? ORDER BY id`
	rows, err := d.db.QueryContext(ctx, q, runID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list trials for run %d: %w", runID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []store.TrialRecord
	for rows.Next() {
		var (
			t        store.TrialRecord
			config   []byte
			fidelity []byte
			timeline []byte
			workload sql.NullString
			mode     sql.NullString
			phase    sql.NullString
			validity sql.NullString
			created  nullTime
		)
		if err := rows.Scan(&t.ID, &t.RunID, &t.ConfigHash, &config, &workload, &fidelity,
			&t.Rep, &mode, &phase, &validity, &timeline, &created); err != nil {
			return nil, fmt.Errorf("sqlite: scan trial for run %d: %w", runID, err)
		}
		t.Config = rawOrNil(config)
		t.Fidelity = rawOrNil(fidelity)
		t.Timeline = rawOrNil(timeline)
		t.Workload = workload.String
		t.Mode = mode.String
		t.Phase = phase.String
		t.Validity = validity.String
		if created.Valid {
			t.CreatedAt = created.Time
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate trials for run %d: %w", runID, err)
	}
	return out, nil
}

// ListSLIValuesForRun returns every SLI value across all trials of a run.
func (d *DB) ListSLIValuesForRun(ctx context.Context, runID int64) ([]store.SLIValueRecord, error) {
	const q = `
SELECT s.id, s.trial_id, s.name, s.provider, s.class, s.value, s.query, s.evaluated_at
FROM sli_values s
JOIN trials t ON t.id = s.trial_id
WHERE t.run_id = ?
ORDER BY s.id`
	rows, err := d.db.QueryContext(ctx, q, runID)
	if err != nil {
		return nil, fmt.Errorf("sqlite: list sli values for run %d: %w", runID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []store.SLIValueRecord
	for rows.Next() {
		var (
			v         store.SLIValueRecord
			provider  sql.NullString
			class     sql.NullString
			query     sql.NullString
			value     sql.NullFloat64
			evaluated nullTime
		)
		if err := rows.Scan(&v.ID, &v.TrialID, &v.Name, &provider, &class, &value, &query, &evaluated); err != nil {
			return nil, fmt.Errorf("sqlite: scan sli value for run %d: %w", runID, err)
		}
		v.Provider = provider.String
		v.Class = class.String
		v.Query = query.String
		v.Value = value.Float64
		if evaluated.Valid {
			v.EvaluatedAt = evaluated.Time
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("sqlite: iterate sli values for run %d: %w", runID, err)
	}
	return out, nil
}

// --- binding / scanning helpers -------------------------------------------------

// jsonNull binds a nullable JSON (TEXT) column: empty → SQL NULL, else TEXT.
func jsonNull(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

// jsonReq binds a NOT NULL JSON (TEXT) column: empty → the JSON literal "null".
func jsonReq(raw json.RawMessage) any {
	if len(raw) == 0 {
		return "null"
	}
	return string(raw)
}

// nullableTime binds a nullable timestamp: nil → SQL NULL, else the UTC value.
func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UTC()
}

// rawOrNil converts scanned bytes to json.RawMessage, preserving nil for NULL.
func rawOrNil(b []byte) json.RawMessage {
	if b == nil {
		return nil
	}
	return json.RawMessage(b)
}

// nullTime scans SQLite timestamp columns that may arrive as time.Time, string,
// []byte, or unix int64 depending on how the value was written.
type nullTime struct {
	Time  time.Time
	Valid bool
}

func (n *nullTime) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		n.Time, n.Valid = time.Time{}, false
		return nil
	case time.Time:
		n.Time, n.Valid = v, true
		return nil
	case []byte:
		return n.parse(string(v))
	case string:
		return n.parse(v)
	case int64:
		n.Time, n.Valid = time.Unix(v, 0).UTC(), true
		return nil
	default:
		return fmt.Errorf("sqlite: cannot scan %T into timestamp", src)
	}
}

func (n *nullTime) parse(s string) error {
	if s == "" {
		n.Time, n.Valid = time.Time{}, false
		return nil
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999999-07:00",
		"2006-01-02 15:04:05.999999999",
		"2006-01-02 15:04:05-07:00",
		"2006-01-02 15:04:05",
		time.RFC3339Nano,
		time.RFC3339,
	} {
		if t, err := time.Parse(layout, s); err == nil {
			n.Time, n.Valid = t, true
			return nil
		}
	}
	return fmt.Errorf("sqlite: cannot parse timestamp %q", s)
}

func (n nullTime) ptr() *time.Time {
	if !n.Valid {
		return nil
	}
	t := n.Time
	return &t
}

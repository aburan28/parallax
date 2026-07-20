-- parallax results database v1 (DESIGN.md §15) — SQLite dialect (--local mode).
-- Same schema semantics as postgres; JSONB→TEXT, BIGSERIAL→INTEGER AUTOINCREMENT,
-- TIMESTAMPTZ→TIMESTAMP, DOUBLE PRECISION→REAL, now()→CURRENT_TIMESTAMP.

CREATE TABLE IF NOT EXISTS studies (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT      NOT NULL,
    namespace  TEXT      NOT NULL,
    spec       TEXT      NOT NULL,
    spec_hash  TEXT      NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (namespace, name, spec_hash)
);

CREATE TABLE IF NOT EXISTS runs (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    study_id     INTEGER   NOT NULL REFERENCES studies (id) ON DELETE CASCADE,
    seed         INTEGER   NOT NULL DEFAULT 0,
    fingerprint  TEXT,
    phase        TEXT      NOT NULL DEFAULT 'Pending',
    started_at   TIMESTAMP,
    completed_at TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_runs_study ON runs (study_id);

CREATE TABLE IF NOT EXISTS trials (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id      INTEGER   NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    config_hash TEXT      NOT NULL,
    config      TEXT,
    workload    TEXT,
    fidelity    TEXT,
    rep         INTEGER   NOT NULL DEFAULT 0,
    mode        TEXT,
    phase       TEXT,
    validity    TEXT,
    timeline    TEXT,
    created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE (run_id, config_hash, rep)
);
CREATE INDEX IF NOT EXISTS idx_trials_run ON trials (run_id);

CREATE TABLE IF NOT EXISTS sli_values (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    trial_id     INTEGER   NOT NULL REFERENCES trials (id) ON DELETE CASCADE,
    name         TEXT      NOT NULL,
    provider     TEXT,
    class        TEXT,
    value        REAL,
    query        TEXT,
    evaluated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_sli_values_trial ON sli_values (trial_id);
CREATE INDEX IF NOT EXISTS idx_sli_values_name ON sli_values (name);

CREATE TABLE IF NOT EXISTS observations (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id          INTEGER NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    seq             INTEGER NOT NULL,
    config_hash     TEXT    NOT NULL,
    fidelity        TEXT,
    objective       REAL,
    budget_counters TEXT
);
CREATE INDEX IF NOT EXISTS idx_observations_run ON observations (run_id);

CREATE TABLE IF NOT EXISTS decisions (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id     INTEGER   NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    stage      TEXT      NOT NULL,
    record     TEXT      NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS validations (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id         INTEGER NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    candidate_hash TEXT    NOT NULL,
    stats          TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS scenarios (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id         INTEGER NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    candidate_hash TEXT    NOT NULL,
    scenario       TEXT    NOT NULL,
    verdict        TEXT    NOT NULL
);

CREATE TABLE IF NOT EXISTS promotions (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id       INTEGER   NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    artifact_ref TEXT,
    approver     TEXT,
    approved_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS artifacts (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    trial_id INTEGER NOT NULL REFERENCES trials (id) ON DELETE CASCADE,
    kind     TEXT    NOT NULL,
    uri      TEXT    NOT NULL,
    digest   TEXT,
    bytes    INTEGER
);
CREATE INDEX IF NOT EXISTS idx_artifacts_trial ON artifacts (trial_id);

CREATE TABLE IF NOT EXISTS datasets (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT NOT NULL UNIQUE,
    manifest    TEXT,
    id_digest   TEXT,
    verified_at TIMESTAMP
);

CREATE TABLE IF NOT EXISTS plugin_audit (
    id     INTEGER PRIMARY KEY AUTOINCREMENT,
    plugin TEXT      NOT NULL,
    kind   TEXT,
    digest TEXT,
    action TEXT      NOT NULL,
    actor  TEXT,
    at     TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_plugin_audit_plugin ON plugin_audit (plugin);

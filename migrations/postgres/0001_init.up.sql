-- parallax results database v1 (DESIGN.md §15) — PostgreSQL dialect.
-- Rows are immutable after their trial completes; re-analysis inserts new decisions.

CREATE TABLE IF NOT EXISTS studies (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT        NOT NULL,
    namespace  TEXT        NOT NULL,
    spec       JSONB       NOT NULL,
    spec_hash  TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (namespace, name, spec_hash)
);

CREATE TABLE IF NOT EXISTS runs (
    id           BIGSERIAL PRIMARY KEY,
    study_id     BIGINT      NOT NULL REFERENCES studies (id) ON DELETE CASCADE,
    seed         BIGINT      NOT NULL DEFAULT 0,
    fingerprint  JSONB,
    phase        TEXT        NOT NULL DEFAULT 'Pending',
    started_at   TIMESTAMPTZ,
    completed_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_runs_study ON runs (study_id);

CREATE TABLE IF NOT EXISTS trials (
    id          BIGSERIAL PRIMARY KEY,
    run_id      BIGINT      NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    config_hash TEXT        NOT NULL,
    config      JSONB,
    workload    TEXT,
    fidelity    JSONB,
    rep         INT         NOT NULL DEFAULT 0,
    mode        TEXT,
    phase       TEXT,
    validity    TEXT,
    timeline    JSONB,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Idempotency key for crash-safe resume (DESIGN.md §15).
    UNIQUE (run_id, config_hash, rep)
);
CREATE INDEX IF NOT EXISTS idx_trials_run ON trials (run_id);

CREATE TABLE IF NOT EXISTS sli_values (
    id           BIGSERIAL PRIMARY KEY,
    trial_id     BIGINT      NOT NULL REFERENCES trials (id) ON DELETE CASCADE,
    name         TEXT        NOT NULL,
    provider     TEXT,
    class        TEXT,
    value        DOUBLE PRECISION,
    query        TEXT,
    evaluated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_sli_values_trial ON sli_values (trial_id);
CREATE INDEX IF NOT EXISTS idx_sli_values_name ON sli_values (name);

CREATE TABLE IF NOT EXISTS observations (
    id              BIGSERIAL PRIMARY KEY,
    run_id          BIGINT NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    seq             INT    NOT NULL,
    config_hash     TEXT   NOT NULL,
    fidelity        JSONB,
    objective       DOUBLE PRECISION,
    budget_counters JSONB
);
CREATE INDEX IF NOT EXISTS idx_observations_run ON observations (run_id);

CREATE TABLE IF NOT EXISTS decisions (
    id         BIGSERIAL PRIMARY KEY,
    run_id     BIGINT      NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    stage      TEXT        NOT NULL,
    record     JSONB       NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS validations (
    id             BIGSERIAL PRIMARY KEY,
    run_id         BIGINT NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    candidate_hash TEXT   NOT NULL,
    stats          JSONB  NOT NULL
);

CREATE TABLE IF NOT EXISTS scenarios (
    id             BIGSERIAL PRIMARY KEY,
    run_id         BIGINT NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    candidate_hash TEXT   NOT NULL,
    scenario       TEXT   NOT NULL,
    verdict        JSONB  NOT NULL
);

CREATE TABLE IF NOT EXISTS promotions (
    id           BIGSERIAL PRIMARY KEY,
    run_id       BIGINT      NOT NULL REFERENCES runs (id) ON DELETE CASCADE,
    artifact_ref TEXT,
    approver     TEXT,
    approved_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS artifacts (
    id       BIGSERIAL PRIMARY KEY,
    trial_id BIGINT NOT NULL REFERENCES trials (id) ON DELETE CASCADE,
    kind     TEXT   NOT NULL,
    uri      TEXT   NOT NULL,
    digest   TEXT,
    bytes    BIGINT
);
CREATE INDEX IF NOT EXISTS idx_artifacts_trial ON artifacts (trial_id);

CREATE TABLE IF NOT EXISTS datasets (
    id          BIGSERIAL PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    manifest    JSONB,
    id_digest   TEXT,
    verified_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS plugin_audit (
    id     BIGSERIAL PRIMARY KEY,
    plugin TEXT        NOT NULL,
    kind   TEXT,
    digest TEXT,
    action TEXT        NOT NULL,
    actor  TEXT,
    at     TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_plugin_audit_plugin ON plugin_audit (plugin);

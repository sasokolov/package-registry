-- +goose Up
-- Hosted coordinates: the source of truth for locally published packages.
-- The S3 manifest object is a projection of these rows (see
-- docs/geo-replication.md); read-path falls back to the projection when the
-- database is down (invariant 7).
CREATE TABLE hosted_manifests (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    feed         TEXT NOT NULL,
    path         TEXT NOT NULL,
    coordinate   TEXT NOT NULL,
    sha256       TEXT NOT NULL,
    size         BIGINT NOT NULL,
    checksums    JSONB NOT NULL DEFAULT '{}'::jsonb,
    metadata     JSONB NOT NULL DEFAULT '{}'::jsonb,
    mutable      BOOLEAN NOT NULL DEFAULT false,
    -- Provenance from day one: geo replication merges on these (Phase 7).
    origin       TEXT NOT NULL DEFAULT 'publish',
    site         TEXT NOT NULL,
    published_by TEXT NOT NULL,
    published_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Invariant 4 is enforced here, not by an advisory lock: the unique
    -- constraint is what makes a concurrent republish fail deterministically.
    CONSTRAINT hosted_manifests_feed_path_key UNIQUE (feed, path)
);
CREATE INDEX hosted_manifests_feed_coord_idx ON hosted_manifests (feed, coordinate);
CREATE INDEX hosted_manifests_sha256_idx ON hosted_manifests (sha256);

-- Quarantine is keyed by (feed, coordinate) and consulted on the read path.
-- The Phase 1 table had a bare coordinate key and no feed: replace it.
DROP TABLE IF EXISTS quarantine;
CREATE TABLE quarantine (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    feed        TEXT NOT NULL,
    coordinate  TEXT NOT NULL,
    reason      TEXT NOT NULL,
    detail      TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    released_at TIMESTAMPTZ,
    CONSTRAINT quarantine_feed_coord_key UNIQUE (feed, coordinate)
);
CREATE INDEX quarantine_active_idx ON quarantine (feed, coordinate) WHERE released_at IS NULL;

-- Shared verdict cache for policies that consult external services
-- (policies/osv today): keeps builds fast across replicas and lets a policy
-- fail open on an upstream outage without hammering it. Generic on purpose:
-- the core knows no policy specifics.
CREATE TABLE policy_verdicts (
    namespace  TEXT NOT NULL,
    key        TEXT NOT NULL,
    value      TEXT NOT NULL,
    checked_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (namespace, key)
);

-- Token rows gain an update timestamp: geo replication of revocations
-- merges on (hash, updated_at).
ALTER TABLE tokens ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT now();

-- +goose Down
ALTER TABLE tokens DROP COLUMN updated_at;
DROP TABLE policy_verdicts;
DROP TABLE quarantine;
DROP TABLE hosted_manifests;
CREATE TABLE quarantine (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    coordinate  TEXT NOT NULL UNIQUE,
    reason      TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    released_at TIMESTAMPTZ
);

-- +goose Up
-- Geo replication (docs/geo-replication.md): sites converge through an
-- append-only journal of events, pulled by peers over an authenticated
-- internal API. Everything here is site-local; peers exchange rows, never
-- database connections.

-- Stable site identity. A site name is configuration, but the UUID is
-- generated once here so a cloned config cannot masquerade as another site.
CREATE TABLE site_identity (
    id         BOOLEAN PRIMARY KEY DEFAULT true CHECK (id),
    site       TEXT NOT NULL,
    site_uuid  UUID NOT NULL DEFAULT gen_random_uuid(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Hybrid logical clock plus the journal sequence. Both are handed out by
-- repl_hlc_next() under a row lock, which is what makes journal sequence
-- order identical to commit order: a paginating reader can never skip an
-- entry that commits later with a lower sequence.
CREATE TABLE hlc_state (
    id       BOOLEAN PRIMARY KEY DEFAULT true CHECK (id),
    wall_ms  BIGINT NOT NULL DEFAULT 0,
    logical  BIGINT NOT NULL DEFAULT 0,
    last_seq BIGINT NOT NULL DEFAULT 0
);
INSERT INTO hlc_state (id) VALUES (true);

-- +goose StatementBegin
CREATE FUNCTION repl_hlc_next() RETURNS TABLE(seq BIGINT, hlc_wall BIGINT, hlc_logical BIGINT) AS $$
DECLARE
    now_ms BIGINT := (EXTRACT(EPOCH FROM clock_timestamp()) * 1000)::BIGINT;
BEGIN
    -- The row lock serializes sequence allocation with the caller's
    -- transaction: entries become visible in sequence order.
    UPDATE hlc_state
       SET wall_ms  = GREATEST(wall_ms, now_ms),
           logical  = CASE WHEN now_ms > wall_ms THEN 0 ELSE logical + 1 END,
           last_seq = last_seq + 1
     WHERE id
    RETURNING last_seq, wall_ms, logical INTO seq, hlc_wall, hlc_logical;
    RETURN NEXT;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose StatementBegin
-- repl_hlc_recv advances the clock past a timestamp observed from a peer,
-- so locally generated timestamps stay causally after applied events.
CREATE FUNCTION repl_hlc_recv(peer_wall BIGINT, peer_logical BIGINT) RETURNS VOID AS $$
BEGIN
    UPDATE hlc_state
       SET wall_ms = GREATEST(wall_ms, peer_wall),
           logical = CASE
               WHEN peer_wall > wall_ms THEN peer_logical + 1
               WHEN peer_wall = wall_ms THEN GREATEST(logical, peer_logical) + 1
               ELSE logical
           END
     WHERE id;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- The journal itself: local events get origin_site = this site, applied
-- peer events keep their origin so the mesh does not loop.
CREATE TABLE repl_journal (
    origin_site TEXT   NOT NULL,
    origin_seq  BIGINT NOT NULL,
    kind        TEXT   NOT NULL,
    payload     JSONB  NOT NULL,
    hlc_wall    BIGINT NOT NULL,
    hlc_logical BIGINT NOT NULL,
    schema_version INT NOT NULL DEFAULT 1,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (origin_site, origin_seq)
);
CREATE INDEX repl_journal_local_idx ON repl_journal (origin_seq) WHERE origin_seq > 0;

-- How far this site has applied each (peer, origin) stream.
CREATE TABLE repl_cursors (
    peer        TEXT   NOT NULL,
    origin_site TEXT   NOT NULL,
    applied_seq BIGINT NOT NULL DEFAULT 0,
    -- durable_seq trails applied_seq until every blob referenced up to that
    -- point exists locally: it is the honest RPO of this site.
    durable_seq BIGINT NOT NULL DEFAULT 0,
    last_ok_at  TIMESTAMPTZ,
    last_error  TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (peer, origin_site)
);

-- Cross-site publish conflicts (rule K1): both sides recorded, the
-- coordinate quarantined until an operator resolves it.
CREATE TABLE publish_conflicts (
    id           BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    feed         TEXT NOT NULL,
    path         TEXT NOT NULL,
    coordinate   TEXT NOT NULL,
    winner_sha256 TEXT NOT NULL,
    loser_sha256  TEXT NOT NULL,
    winner_site  TEXT NOT NULL,
    loser_site   TEXT NOT NULL,
    detected_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    resolved_at  TIMESTAMPTZ,
    resolved_sha256 TEXT
);
CREATE INDEX publish_conflicts_open_idx ON publish_conflicts (feed, path) WHERE resolved_at IS NULL;

-- Events that could not be applied (unknown kind, blob unavailable, clock
-- skew): parked with the reason instead of blocking the stream.
CREATE TABLE repl_parked (
    origin_site TEXT   NOT NULL,
    origin_seq  BIGINT NOT NULL,
    kind        TEXT   NOT NULL,
    payload     JSONB  NOT NULL,
    reason      TEXT   NOT NULL,
    parked_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    retries     INT    NOT NULL DEFAULT 0,
    PRIMARY KEY (origin_site, origin_seq)
);

-- A coordinate can be quarantined for several independent reasons (a
-- cross-site conflict AND a manual takedown). Keying by reason makes
-- quarantine a set: adding and releasing reasons commute, so replicas
-- converge whatever order the events arrive in.
ALTER TABLE quarantine DROP CONSTRAINT quarantine_feed_coord_key;
ALTER TABLE quarantine ADD CONSTRAINT quarantine_feed_coord_reason_key
    UNIQUE (feed, coordinate, reason);

-- +goose Down
ALTER TABLE quarantine DROP CONSTRAINT quarantine_feed_coord_reason_key;
ALTER TABLE quarantine ADD CONSTRAINT quarantine_feed_coord_key UNIQUE (feed, coordinate);
DROP TABLE repl_parked;
DROP TABLE publish_conflicts;
DROP TABLE repl_cursors;
DROP TABLE repl_journal;
DROP FUNCTION repl_hlc_recv(BIGINT, BIGINT);
DROP FUNCTION repl_hlc_next();
DROP TABLE hlc_state;
DROP TABLE site_identity;

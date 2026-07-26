-- +goose Up
-- Quarantine state has to converge like everything else that replicates:
-- an event set and its release can arrive in either order, and a release
-- that lands before the set it lifts must not be lost. The row therefore
-- becomes a last-writer-wins register stamped with the originating HLC,
-- instead of a conditional UPDATE that silently matches nothing.
ALTER TABLE quarantine
    ADD COLUMN hlc_wall    BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN hlc_logical BIGINT NOT NULL DEFAULT 0;

-- +goose StatementBegin
-- repl_hlc_now stamps a local state change without consuming a journal
-- sequence, so locally-decided quarantines order correctly against
-- replicated ones.
CREATE FUNCTION repl_hlc_now() RETURNS TABLE(hlc_wall BIGINT, hlc_logical BIGINT) AS $$
DECLARE
    now_ms BIGINT := (EXTRACT(EPOCH FROM clock_timestamp()) * 1000)::BIGINT;
BEGIN
    UPDATE hlc_state
       SET wall_ms = GREATEST(wall_ms, now_ms),
           logical = CASE WHEN now_ms > wall_ms THEN 0 ELSE logical + 1 END
     WHERE id
    RETURNING wall_ms, logical INTO hlc_wall, hlc_logical;
    RETURN NEXT;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- Cross-site publish conflicts must carry enough of each side's manifest
-- that resolving to either one restores a consistent row: keeping a digest
-- without its size and checksums would advertise one artifact's integrity
-- for another's bytes.
ALTER TABLE publish_conflicts
    ADD COLUMN winner_meta JSONB NOT NULL DEFAULT '{}'::jsonb,
    ADD COLUMN loser_meta  JSONB NOT NULL DEFAULT '{}'::jsonb;

-- A resolved coordinate is a terminal decision: a conflicting publish that
-- arrives later (independent per-origin streams reorder freely) must not
-- re-open it. The decision is stored as its own row, stamped with the HLC
-- of the deciding event, and consulted by every later merge.
CREATE TABLE conflict_resolutions (
    feed        TEXT   NOT NULL,
    path        TEXT   NOT NULL,
    coordinate  TEXT   NOT NULL,
    keep_sha256 TEXT   NOT NULL,
    size        BIGINT NOT NULL DEFAULT 0,
    checksums   JSONB  NOT NULL DEFAULT '{}'::jsonb,
    metadata    JSONB  NOT NULL DEFAULT '{}'::jsonb,
    operator    TEXT   NOT NULL DEFAULT '',
    decided_by  TEXT   NOT NULL DEFAULT '',
    hlc_wall    BIGINT NOT NULL DEFAULT 0,
    hlc_logical BIGINT NOT NULL DEFAULT 0,
    decided_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (feed, path)
);

-- A parked event keeps its full identity: without the HLC a retried mutable
-- event would merge as if it were the oldest write in the mesh.
ALTER TABLE repl_parked
    ADD COLUMN hlc_wall       BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN hlc_logical    BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN schema_version INT    NOT NULL DEFAULT 1;

-- Peer identities are pinned durably: a per-process memory of "the UUID I
-- saw first" means every replica and every restart re-decides who a peer
-- is, which is exactly the in-memory correctness state invariant 3 bans.
CREATE TABLE repl_peer_identity (
    peer       TEXT PRIMARY KEY,
    site_uuid  UUID NOT NULL,
    first_seen TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE repl_peer_identity;
ALTER TABLE repl_parked
    DROP COLUMN schema_version, DROP COLUMN hlc_logical, DROP COLUMN hlc_wall;
DROP TABLE conflict_resolutions;
ALTER TABLE publish_conflicts DROP COLUMN loser_meta, DROP COLUMN winner_meta;
DROP FUNCTION repl_hlc_now();
ALTER TABLE quarantine DROP COLUMN hlc_logical, DROP COLUMN hlc_wall;

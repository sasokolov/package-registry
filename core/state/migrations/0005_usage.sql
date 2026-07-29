-- +goose Up
-- What each feed holds and how much it is used.
--
-- Both tables are derived data: losing them costs a number on a screen, not
-- correctness, and nothing on the read path waits for them (invariant 7).
-- They live here rather than in the blob store because they are dynamic
-- state that every replica has to agree on, and because "sum this up" is
-- what a database is for.

-- One row per feed: the inventory, recomputed by a periodic scan.
--
-- Hosted content is counted from hosted_manifests, which is exact and free.
-- Proxy-cached content has no rows anywhere — the cache is in the blob store
-- so that reads survive a database outage — so it is counted by walking
-- manifests/<feed>/, which is why this is a scan and not a view.
CREATE TABLE feed_usage (
    feed             TEXT PRIMARY KEY,
    -- Artifacts is stored objects; packages is distinct coordinates. A
    -- Maven release is one package and several artifacts (jar, pom, sources),
    -- and an operator asking "how many packages" means the former.
    hosted_artifacts BIGINT      NOT NULL DEFAULT 0,
    cached_artifacts BIGINT      NOT NULL DEFAULT 0,
    hosted_packages  BIGINT      NOT NULL DEFAULT 0,
    cached_packages  BIGINT      NOT NULL DEFAULT 0,
    -- Bytes this feed's content occupies. Blobs are content-addressed and
    -- shared, so a blob two feeds point at is counted in both: this answers
    -- "what would this feed cost" rather than "what would deleting it free".
    -- shared_bytes is the part of it another feed also points at, which is
    -- the difference between those two questions.
    hosted_bytes     BIGINT      NOT NULL DEFAULT 0,
    cached_bytes     BIGINT      NOT NULL DEFAULT 0,
    shared_bytes     BIGINT      NOT NULL DEFAULT 0,
    last_ingest_at   TIMESTAMPTZ,
    scanned_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- One row per (feed, source): how much this feed served and from where.
--
-- Counters are accumulated in memory and flushed periodically, so a request
-- never waits for this and a database outage costs at most the unflushed
-- delta. That is the trade this table is for: it is a usage number, not an
-- audit record, and the audit log is where exactness lives.
CREATE TABLE feed_traffic (
    feed       TEXT        NOT NULL,
    -- cache | upstream | stale | local | peer | redirect, plus "ingest" for
    -- bytes pulled from an upstream to fill the cache. Keeping ingest here
    -- rather than in its own table is what makes "bytes saved by the cache"
    -- one query.
    source     TEXT        NOT NULL,
    requests   BIGINT      NOT NULL DEFAULT 0,
    bytes      BIGINT      NOT NULL DEFAULT 0,
    last_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (feed, source)
);

-- What the object store actually holds, once, however many feeds point at
-- it. Per-feed bytes answer "what does this feed cost"; blobs are shared, so
-- adding those up overstates the bill. This row is the bill.
CREATE TABLE usage_site (
    only_row       BOOLEAN     PRIMARY KEY DEFAULT true CHECK (only_row),
    distinct_blobs BIGINT      NOT NULL DEFAULT 0,
    distinct_bytes BIGINT      NOT NULL DEFAULT 0,
    scanned_at     TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE usage_site;
DROP TABLE feed_traffic;
DROP TABLE feed_usage;

-- +goose Up
-- What gets downloaded, by coordinate.
--
-- Feed-level counters answer "is this feed worth its disk"; this answers the
-- next question, "what in it is actually used". It is deliberately a table and
-- not a metric: a registry has an unbounded number of coordinates and a
-- handful of feeds, and a coordinate in a Prometheus label is how a
-- monitoring system falls over. A database row is the right shape for
-- something you sort and take the top of.
--
-- Derived data, like the rest of core/usage: losing it costs a leaderboard.
CREATE TABLE package_downloads (
    feed       TEXT        NOT NULL,
    -- The coordinate as the format module resolved it, e.g.
    -- "maven:com.example:lib@1.0.0" — the same string the audit log and the
    -- access rules use, so a name here can be pasted into either.
    coordinate TEXT        NOT NULL,
    downloads  BIGINT      NOT NULL DEFAULT 0,
    bytes      BIGINT      NOT NULL DEFAULT 0,
    last_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (feed, coordinate)
);

-- The only query this table serves: the top of one feed, or of all of them.
CREATE INDEX package_downloads_top_idx ON package_downloads (feed, downloads DESC);

-- +goose Down
DROP TABLE package_downloads;

-- +goose Up
CREATE TABLE tokens (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    hash       TEXT NOT NULL UNIQUE, -- hex sha256 of the secret; never the secret itself
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at TIMESTAMPTZ
);

CREATE TABLE audit (
    id         BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    identity   TEXT NOT NULL,
    action     TEXT NOT NULL,
    feed       TEXT NOT NULL DEFAULT '',
    coordinate TEXT NOT NULL DEFAULT '',
    decision   TEXT NOT NULL DEFAULT '',
    reason     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX audit_at_idx ON audit (at);

CREATE TABLE publish_sessions (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    feed       TEXT NOT NULL,
    coordinate TEXT NOT NULL,
    identity   TEXT NOT NULL,
    state      TEXT NOT NULL DEFAULT 'open', -- open|committed|aborted
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE quarantine (
    id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    coordinate  TEXT NOT NULL UNIQUE,
    reason      TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    released_at TIMESTAMPTZ
);

-- +goose Down
DROP TABLE quarantine;
DROP TABLE publish_sessions;
DROP TABLE audit;
DROP TABLE tokens;

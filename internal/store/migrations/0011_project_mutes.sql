-- Project mutes. A mute may match by project too: every event filed under
-- it. SQLite cannot change a CHECK, so the table is rebuilt, as 0005 did
-- for checks: new table, copy, drop, rename.
CREATE TABLE mutes_new (
    id         TEXT PRIMARY KEY,   -- mute_ plus a ULID
    source     TEXT,
    key        TEXT,
    project    TEXT,
    until      TEXT NOT NULL,
    created_at TEXT NOT NULL,
    created_by TEXT NOT NULL,      -- the token that made it
    ended_at   TEXT,               -- DELETE /v1/mutes/{id}
    ended_by   TEXT,
    CHECK (source IS NOT NULL OR key IS NOT NULL OR project IS NOT NULL)
);
INSERT INTO mutes_new (id, source, key, until, created_at, created_by, ended_at, ended_by)
    SELECT id, source, key, until, created_at, created_by, ended_at, ended_by FROM mutes;
DROP TABLE mutes;
ALTER TABLE mutes_new RENAME TO mutes;
CREATE INDEX mutes_until ON mutes (until) WHERE ended_at IS NULL;

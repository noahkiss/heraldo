-- Stage 5. Re-alert schedule on incidents: an open incident whose event asked
-- for a re-alert (details.heraldo.realert_s, set by a rule) is pushed again at
-- next_alert_at until it is acked or resolved. alert_event is the event the
-- re-alert pushes again.
ALTER TABLE incidents ADD COLUMN realert_s     INTEGER;
ALTER TABLE incidents ADD COLUMN next_alert_at TEXT;
ALTER TABLE incidents ADD COLUMN alert_event   TEXT;
CREATE INDEX incidents_next_alert ON incidents (next_alert_at) WHERE next_alert_at IS NOT NULL;

-- Mutes hold back pushes, not storage. A mute matches by source, key or
-- both, until it expires or is ended. A row is never deleted.
CREATE TABLE mutes (
    id         TEXT PRIMARY KEY,   -- mute_ plus a ULID
    source     TEXT,
    key        TEXT,
    until      TEXT NOT NULL,
    created_at TEXT NOT NULL,
    created_by TEXT NOT NULL,      -- the token that made it
    ended_at   TEXT,               -- DELETE /v1/mutes/{id}
    ended_by   TEXT,
    CHECK (source IS NOT NULL OR key IS NOT NULL)
);
CREATE INDEX mutes_until ON mutes (until) WHERE ended_at IS NULL;

-- The icon registry: name to image, written with an admin token.
CREATE TABLE icons (
    name         TEXT PRIMARY KEY,
    content_type TEXT NOT NULL,    -- image/png or image/jpeg
    data         BLOB NOT NULL,
    sha256       TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    updated_by   TEXT NOT NULL
);

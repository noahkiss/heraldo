-- The alert hold (docs/api.md, "Hold"). An alert-rung post on the Apprise
-- compatibility ingest waits here until triage releases or suppresses it,
-- or until its deadline forwards it. The stored event never changes; this
-- table is the hold's live state.
CREATE TABLE hold_entries (
    id         TEXT PRIMARY KEY,     -- h-YYYYMMDD-HHMMSS-xxxx
    event_id   TEXT,                 -- the event stored on arrival
    ts         TEXT NOT NULL,        -- arrival
    deadline   TEXT NOT NULL,
    tag        TEXT,
    title      TEXT,
    body       TEXT,
    client     TEXT,                 -- the sender's address
    source     TEXT,
    input      TEXT NOT NULL,        -- the post as a message, JSON, for a forward
    forwarded  INTEGER NOT NULL,     -- 1 once it reached a pushing event
    shadow     INTEGER NOT NULL,
    status     TEXT NOT NULL,        -- held, released, suppressed, expired
    closed_at  TEXT,
    verdict    TEXT,
    reason     TEXT
);
CREATE INDEX hold_entries_status ON hold_entries (status, ts);

-- One value per name. last_poll is the last triage poll of the hold.
CREATE TABLE hold_meta (
    name  TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

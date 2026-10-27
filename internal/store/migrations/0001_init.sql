-- Envelope v1 store. events is append-only: the full stored event as JSON,
-- plus the columns the read API filters on. seq is the arrival order and the
-- list cursor; id is the public, sortable event id.
CREATE TABLE events (
    seq         INTEGER PRIMARY KEY AUTOINCREMENT,
    id          TEXT    NOT NULL UNIQUE,
    received_at TEXT    NOT NULL,
    ts          TEXT    NOT NULL,
    source      TEXT    NOT NULL,
    host        TEXT,
    kind        TEXT    NOT NULL,
    key         TEXT,
    action      TEXT,
    level       TEXT,
    tags        TEXT,   -- JSON array, or NULL
    dedup       TEXT,
    event       TEXT    NOT NULL  -- the stored Event, JSON
);
CREATE INDEX events_source ON events (source, seq);
CREATE INDEX events_host   ON events (host, seq);
CREATE INDEX events_kind   ON events (kind, seq);
CREATE INDEX events_key    ON events (key, seq);
CREATE INDEX events_dedup  ON events (dedup, received_at) WHERE dedup IS NOT NULL;

-- Full-text search over title, summary and body. Contentless: events keeps
-- the text, and rowid is events.seq.
CREATE VIRTUAL TABLE events_fts USING fts5 (title, summary, body, content='');

-- Derived state, maintained in the same transaction as the insert.
CREATE TABLE incidents (
    key         TEXT PRIMARY KEY,
    state       TEXT NOT NULL,     -- open, acked, resolved
    opened_at   TEXT NOT NULL,
    resolved_at TEXT,
    last_event  TEXT NOT NULL
);

CREATE TABLE checkins (
    key        TEXT PRIMARY KEY,
    last_ping  TEXT NOT NULL,
    state      TEXT NOT NULL,      -- running, up, down
    last_event TEXT NOT NULL
);

CREATE TABLE activities (
    key        TEXT PRIMARY KEY,
    state      TEXT NOT NULL,      -- active, ended
    last_event TEXT NOT NULL
);

CREATE TABLE status (
    key        TEXT PRIMARY KEY,
    content    TEXT NOT NULL,      -- merged LiveContent, JSON
    last_event TEXT NOT NULL
);

-- Ids of imported records, so a rerun of an import stores nothing twice.
CREATE TABLE imports (
    origin    TEXT NOT NULL,       -- such as tee
    origin_id TEXT NOT NULL,
    event_id  TEXT NOT NULL,
    PRIMARY KEY (origin, origin_id)
);

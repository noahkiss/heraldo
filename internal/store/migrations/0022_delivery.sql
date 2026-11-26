-- Delivery settings: rows the owner keeps that say how matching events are
-- delivered (banner, silent, digest, drop), whether they count toward the
-- badge, and what level an event that carries none gets. match is the
-- row's Match as JSON. The rows act only while delivery.enabled is on.
CREATE TABLE delivery_settings (
    id            TEXT PRIMARY KEY,
    position      INTEGER NOT NULL DEFAULT 0,
    match         TEXT NOT NULL,
    deliver       TEXT,
    badge         TEXT NOT NULL DEFAULT 'auto',
    default_level TEXT,
    note          TEXT,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    updated_by    TEXT NOT NULL DEFAULT ''
);

-- Each source the server stored an event from, folded to lower case. A
-- source with no reviewed_at is new: its level-less events get
-- delivery.new_source_level. Every source stored before this migration is
-- seeded as reviewed (at its last event), so at cut-over no source is new.
CREATE TABLE sources (
    source      TEXT PRIMARY KEY,
    first_seen  TEXT NOT NULL,
    last_seen   TEXT NOT NULL,
    reviewed_at TEXT
);
INSERT INTO sources (source, first_seen, last_seen, reviewed_at)
    SELECT lower(source), min(received_at), max(received_at), max(received_at)
    FROM events GROUP BY lower(source);

-- The stamp's outcome and badge choice, for the hidden filter, the digest
-- and the badge. NULL on events stored without a stamp: shown, and auto.
ALTER TABLE events ADD COLUMN deliver_as TEXT;
ALTER TABLE events ADD COLUMN badge TEXT;
CREATE INDEX events_deliver_as ON events (deliver_as) WHERE deliver_as IS NOT NULL;

-- An incident's badge choice, from the stamp of its latest event. NULL is
-- auto.
ALTER TABLE incidents ADD COLUMN badge TEXT;

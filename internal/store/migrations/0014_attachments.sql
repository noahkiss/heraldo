-- Attachments: files a sender uploads before the event that names them
-- (POST /v1/attachments, then attachments: [ids] on the event). The bytes
-- live in the database, so a backup carries them. One row per distinct
-- body: sha256 is unique, and a second upload of the same bytes returns the
-- first row. event_id is the first event that named the attachment; a row
-- with none is detached, and the sweep deletes it after 24 hours.
CREATE TABLE attachments (
    id           TEXT PRIMARY KEY,   -- att_ plus a ULID
    event_id     TEXT,               -- the first event that named it; NULL while detached
    content_type TEXT NOT NULL,
    bytes        INTEGER NOT NULL,
    sha256       TEXT NOT NULL UNIQUE,
    body         BLOB NOT NULL,
    created_at   TEXT NOT NULL,
    created_by   TEXT NOT NULL       -- the token that uploaded it
);
CREATE INDEX attachments_detached ON attachments (created_at) WHERE event_id IS NULL;
CREATE INDEX attachments_event ON attachments (event_id) WHERE event_id IS NOT NULL;

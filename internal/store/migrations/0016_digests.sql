-- The morning digest's send log: one row per local day the digest went out.
-- The digest ticker writes the row after it stores the digest event, and
-- skips a day that has one, so a restart never sends a day's digest twice.
-- until_at is the end of the window the digest covered; the next digest
-- starts there.
CREATE TABLE digests (
    day      TEXT PRIMARY KEY,   -- YYYY-MM-DD in digest.tz
    sent_at  TEXT NOT NULL,
    since_at TEXT NOT NULL,
    until_at TEXT NOT NULL,
    event_id TEXT NOT NULL
);

-- Check-ins become checks with a schedule (stage 6). A check is one row,
-- keyed by its slug, which is also the key of its check-in events. A check
-- made through the API has had no ping yet, so last_ping may be NULL; the
-- table is rebuilt for that. Times are in the store's fixed-width format;
-- durations are seconds, except last_duration_ms. A ping key lets a sender
-- ping this one check with no bearer token; only its hash is stored.
CREATE TABLE checks (
    key              TEXT PRIMARY KEY,
    host             TEXT,
    period_s         INTEGER,
    cron             TEXT,
    tz               TEXT,
    grace_s          INTEGER NOT NULL DEFAULT 3600,
    max_runtime_s    INTEGER,
    reminder_s       INTEGER NOT NULL DEFAULT 86400,  -- 0 sends no reminders
    state            TEXT NOT NULL,      -- new, up, started, late, down, paused
    state_since      TEXT NOT NULL,
    down_reason      TEXT,               -- failed, missed, overran
    created_at       TEXT NOT NULL,
    last_ping        TEXT,
    last_event       TEXT,
    started_at       TEXT,               -- the run in progress
    start_rid        TEXT,
    last_success     TEXT,
    last_duration_ms INTEGER,
    last_log_event   TEXT,
    due_at           TEXT,               -- the next ping is due
    reminded_at      TEXT,               -- the incident last opened or reminded
    ping_key_hash    TEXT UNIQUE,        -- SHA-256 hex of the check's ping key
    ping_key_at      TEXT                -- when the key was minted
);

-- Checks the scheduler judges: the rest only change on a ping.
CREATE INDEX checkins_judged ON checks (state) WHERE state IN ('up', 'late', 'started', 'down');

-- Existing check-ins carry over unscheduled: running is now started and
-- unknown (only log pings so far) is new.
INSERT INTO checks (key, grace_s, state, state_since, created_at, last_ping, last_event, last_success, down_reason)
SELECT key, 3600,
       CASE state WHEN 'running' THEN 'started' WHEN 'unknown' THEN 'new' ELSE state END,
       last_ping, last_ping, last_ping, last_event,
       CASE WHEN state = 'up' THEN last_ping END,
       CASE WHEN state = 'down' THEN 'failed' END
FROM checkins;

DROP TABLE checkins;
ALTER TABLE checks RENAME TO checkins;

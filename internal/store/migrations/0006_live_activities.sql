-- Live Activities. activities keeps one row per progress key: the current
-- activity on that key, its merged content and how it ended. A start after
-- an end makes a new activity on the same key, with a new id.
ALTER TABLE activities ADD COLUMN id TEXT;            -- act_ plus a ULID
ALTER TABLE activities ADD COLUMN source TEXT;
ALTER TABLE activities ADD COLUMN content TEXT;       -- merged LiveContent, JSON
ALTER TABLE activities ADD COLUMN started_at TEXT;
ALTER TABLE activities ADD COLUMN updated_at TEXT;    -- the sender's last event
ALTER TABLE activities ADD COLUMN interval_ms INTEGER; -- the gap before the last update
ALTER TABLE activities ADD COLUMN ended_at TEXT;
ALTER TABLE activities ADD COLUMN end_reason TEXT;    -- sender, abandoned
ALTER TABLE activities ADD COLUMN level TEXT;         -- the start event's level

-- The push-to-start token of each device: ActivityKit's
-- pushToStartToken for the app's attributes type.
ALTER TABLE devices ADD COLUMN live_activity_token TEXT;

-- One Live Activity on one device. An activity runs as a chain of these:
-- the server ends each one before Apple's 8 hour limit and starts the next
-- with the same content. id is in the activity's attributes, so the app can
-- report the update token and the end of the right one.
CREATE TABLE live_instances (
    id           TEXT PRIMARY KEY,     -- lai_ plus a ULID
    activity_id  TEXT NOT NULL,
    key          TEXT NOT NULL,
    device_id    TEXT NOT NULL,
    chain        INTEGER NOT NULL,     -- 0 for the first, then 1, 2, ...
    state        TEXT NOT NULL,        -- starting, active, ended, dismissed, never-started, dead
    end_reason   TEXT,
    update_token TEXT,                 -- the activity's push token, from the app
    requested_at TEXT NOT NULL,        -- the push-to-start went out
    confirmed_at TEXT,                 -- the app reported the update token
    ended_at     TEXT,
    last_push_at TEXT
);
CREATE INDEX live_instances_activity ON live_instances (activity_id, device_id, chain);
CREATE INDEX live_instances_open ON live_instances (state) WHERE state IN ('starting', 'active');

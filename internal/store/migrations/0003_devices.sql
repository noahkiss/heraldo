-- Devices the apps register for push, and the push outcome per device per
-- event. A device row is never deleted: removing or retiring a device sets
-- retired_at, and registering the same token again brings it back.
CREATE TABLE devices (
    id              TEXT PRIMARY KEY,     -- server id, dev_ plus a ULID
    token           TEXT NOT NULL UNIQUE, -- APNs device token, lower-case hex
    environment     TEXT NOT NULL,        -- sandbox or production
    topic           TEXT NOT NULL,        -- the app's bundle id, apns-topic
    name            TEXT,
    platform        TEXT,
    app_version     TEXT,
    alerts          INTEGER,              -- permission health: 1 on, 0 off, NULL unknown
    time_sensitive  INTEGER,
    live_activities INTEGER,
    registered_at   TEXT NOT NULL,
    updated_at      TEXT NOT NULL,
    updated_by      TEXT NOT NULL,        -- the token that last wrote the row
    retired_at      TEXT,
    retired_reason  TEXT
);

CREATE TABLE deliveries (
    event_id  TEXT NOT NULL,
    device_id TEXT NOT NULL,
    outcome   TEXT NOT NULL,             -- sent, failed, skipped
    level     TEXT,                      -- the interruption level the push carried
    at        TEXT NOT NULL,
    error     TEXT,
    apns_id   TEXT,
    attempts  INTEGER NOT NULL,
    PRIMARY KEY (event_id, device_id)
);

-- Why-pushed: the push pipeline's trace. Each stage the event passed
-- through writes one row, in stage order. The rows are the event's latest
-- push decision: a new decision of the event (a re-alert that pushes, a
-- start-up sweep) replaces them, as it replaces the deliveries.
CREATE TABLE push_decisions (
    event_id TEXT NOT NULL,
    n        INTEGER NOT NULL,     -- stage order, from 0
    stage    TEXT NOT NULL,        -- hold, expiry, table, replace, devices, mute, inhibit, deliver, ...
    outcome  TEXT NOT NULL,        -- pass, downgrade, delay, merge, drop
    reason   TEXT NOT NULL,        -- one line a person reads
    detail   TEXT,                 -- JSON object, or NULL
    at       TEXT NOT NULL,
    PRIMARY KEY (event_id, n)
);

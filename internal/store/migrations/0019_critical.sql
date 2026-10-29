-- Critical alerts and emergency escalation.
-- critical_alerts is the device's critical-alerts permission, as the app
-- reports it: NULL is unknown (a build without the entitlement never
-- reports it). Only a device with 1 gets interruption-level critical.
ALTER TABLE devices ADD COLUMN critical_alerts INTEGER;

-- One row per critical event sent to Pushover's emergency priority. The
-- receipt is Pushover's id for the retrying alert; an ack or resolve of the
-- key cancels it.
CREATE TABLE escalations (
    event_id      TEXT PRIMARY KEY,
    key           TEXT NOT NULL,      -- the event's key; '' for none
    provider      TEXT NOT NULL,      -- pushover
    receipt       TEXT NOT NULL,
    sent_at       TEXT NOT NULL,
    expires_at    TEXT NOT NULL,      -- when the provider stops retrying
    cancelled_at  TEXT,
    cancel_reason TEXT
);
CREATE INDEX escalations_open ON escalations (key) WHERE cancelled_at IS NULL;

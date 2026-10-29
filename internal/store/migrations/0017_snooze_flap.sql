-- Snooze and flap grouping (0.7).
--
-- Snooze reuses mutes. A mute with remind = 1 is a snooze: when it ends,
-- by its until or by DELETE, and its key's incident is still open, the
-- pusher pushes the key's last event again at its level. reminded_at is set
-- when the pusher took the reminder, so it fires once.
ALTER TABLE mutes ADD COLUMN remind      INTEGER NOT NULL DEFAULT 0;
ALTER TABLE mutes ADD COLUMN reminded_at TEXT;
CREATE INDEX mutes_remind ON mutes (until) WHERE remind = 1 AND reminded_at IS NULL;

-- Flap state on incidents. With flap.enabled, a key that opens flap.opens
-- times within flap.window is flapping from flap_since. flap_count counts
-- its opens from the first one in that window. flap_opens holds the recent
-- open times (a JSON array, trimmed to the window); flap_last is the last
-- open or resolve. A flapping incident quiet for flap.settle is no longer
-- flapping: readers check flap_last, and the next open starts over.
ALTER TABLE incidents ADD COLUMN flapping   INTEGER NOT NULL DEFAULT 0;
ALTER TABLE incidents ADD COLUMN flap_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE incidents ADD COLUMN flap_since TEXT;
ALTER TABLE incidents ADD COLUMN flap_opens TEXT;
ALTER TABLE incidents ADD COLUMN flap_last  TEXT;

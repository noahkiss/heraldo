-- Live Activity broadcast channels (iOS 18). An instance started with
-- input-push-channel gets its updates and its end on its own APNs channel,
-- so the device never has to report an update token. channel_id is APNs's
-- id for it; channel_deleted_at is when the server deleted the channel
-- after the instance closed.
ALTER TABLE live_instances ADD COLUMN channel_id TEXT;
ALTER TABLE live_instances ADD COLUMN channel_deleted_at TEXT;
CREATE INDEX live_instances_channel ON live_instances (state)
    WHERE channel_id IS NOT NULL AND channel_deleted_at IS NULL;

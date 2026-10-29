-- The Focus filter a device reports (PUT /v1/devices/{id}, focus_filter):
-- JSON {name, projects, break_through, updated_at}, NULL when none is in
-- force. The pusher lowers that device's pushes above it to active. A
-- standby copies it with the device.
ALTER TABLE devices ADD COLUMN focus_filter TEXT;

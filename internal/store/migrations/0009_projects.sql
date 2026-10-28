-- Projects. An event may name the project it belongs to; the derived rows
-- keep the project of their key's last event that named one, so a filter on
-- them needs no join and an event with no project can inherit its key's.
ALTER TABLE events ADD COLUMN project TEXT;
CREATE INDEX events_project ON events (project, seq);
ALTER TABLE incidents  ADD COLUMN project TEXT;
ALTER TABLE checkins   ADD COLUMN project TEXT;
ALTER TABLE activities ADD COLUMN project TEXT;
ALTER TABLE status     ADD COLUMN project TEXT;

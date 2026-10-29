-- Routine events. Check-in pings that did not fail, progress and status
-- events never push, and clients keep them out of the main stream with
-- GET /v1/events?routine=exclude. They can be most of the store, so a list
-- of the rest, newest first, walks this index instead of every event. The
-- condition is store.RoutineWhere negated; the two must stay the same
-- expression or SQLite will not use the index.
CREATE INDEX events_main ON events (seq)
    WHERE NOT (kind IN ('progress', 'status') OR (kind = 'checkin' AND NOT (coalesce(action, '') = 'fail'
        OR (coalesce(action, '') != 'log' AND coalesce(json_extract(event, '$.checkin.exit_code'), 0) != 0))));

-- A standby server's own state, one value per name: how far it has handed
-- its events back to the primary, and whether it raised the primary-down
-- alert. A primary never writes here.
CREATE TABLE standby_state (
    name  TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

-- Saved views: a named event filter that every client shares, and an
-- optional bundle that folds a burst of the view's pushes into one
-- notification. filter and bundle are JSON objects (store.ViewFilter,
-- store.ViewBundle); bundle is NULL when the view does not bundle. name is
-- unique without regard to case.
CREATE TABLE views (
    id         TEXT PRIMARY KEY,     -- view_ plus a ULID
    name       TEXT NOT NULL,
    filter     TEXT NOT NULL,        -- JSON object
    bundle     TEXT,                 -- JSON object, or NULL
    position   INTEGER NOT NULL DEFAULT 0,
    icon       TEXT,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    created_by TEXT NOT NULL
);
CREATE UNIQUE INDEX views_name ON views (name COLLATE NOCASE);

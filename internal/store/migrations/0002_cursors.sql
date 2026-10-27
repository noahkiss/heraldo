-- Read cursors. One cursor is one unread state, shared by every client that
-- names it: the CLI on each machine and the apps read one inbox. name is the
-- cursor's name, such as inbox; updated_by is the token that last moved it.
CREATE TABLE cursors (
    name       TEXT    PRIMARY KEY,
    seq        INTEGER NOT NULL,
    updated_at TEXT    NOT NULL,
    updated_by TEXT    NOT NULL
);

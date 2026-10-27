-- Read cursors, one per reading client. owner is the token's name, so a
-- token reads and moves only its own clients' cursors; name is the client's
-- own label for itself, such as a machine name.
CREATE TABLE cursors (
    owner      TEXT    NOT NULL,
    name       TEXT    NOT NULL,
    seq        INTEGER NOT NULL,
    updated_at TEXT    NOT NULL,
    PRIMARY KEY (owner, name)
);

-- Server settings: one row per key the operator changed through the API,
-- the CLI or MCP. A key with no row takes its built-in default. A launch
-- flag or env var still wins over a row (internal/settings). value is the
-- JSON value; a secret's value is {"enc": "<base64 nonce and AES-GCM
-- ciphertext>"}, never the plain text.
CREATE TABLE settings (
    key        TEXT PRIMARY KEY,
    value      TEXT NOT NULL,      -- JSON
    updated_at TEXT NOT NULL,
    updated_by TEXT NOT NULL       -- the token that wrote it
);

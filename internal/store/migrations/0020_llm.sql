-- The LLM: a log of every call to the provider, and guidance.
--
-- llm_calls stores what came back, never the prompt: the prompt can be
-- rebuilt from the ids and versions. input_hash is
-- sha256(role, model, prompt version, guidance versions, normalized input);
-- a status ok row less than 24 h old with the same hash is the cache.
-- status is ok, error, timeout, invalid (the output failed validation) or
-- cached (served from an earlier row). A call the circuit or a budget
-- refused sends nothing and leaves no row. Rows older than 30 days go.
CREATE TABLE llm_calls (
    id             TEXT PRIMARY KEY,  -- llm_ plus a ULID
    role           TEXT NOT NULL,     -- chat, decisions, test
    model          TEXT NOT NULL,
    base_host      TEXT NOT NULL,     -- the endpoint's host, never a path or token
    at             TEXT NOT NULL,
    latency_ms     INTEGER NOT NULL DEFAULT 0,
    tokens_in      INTEGER NOT NULL DEFAULT 0,
    tokens_out     INTEGER NOT NULL DEFAULT 0,
    input_hash     TEXT NOT NULL,
    prompt_version TEXT NOT NULL,
    status         TEXT NOT NULL,
    output         TEXT,              -- JSON, or NULL
    error          TEXT
);
CREATE INDEX llm_calls_at ON llm_calls (at);
CREATE INDEX llm_calls_hash ON llm_calls (input_hash, at);

-- Guidance: free text that informs the LLM's wording and grouping, never a
-- floor or a rule. scope is global, project:<p>, source:<s> or key:<regex>.
-- version starts at 1 and goes up on each edit, so the cache key changes.
CREATE TABLE guidance (
    id         TEXT PRIMARY KEY,      -- gd_ plus a ULID
    scope      TEXT NOT NULL,
    text       TEXT NOT NULL,
    enabled    INTEGER NOT NULL DEFAULT 1,
    version    INTEGER NOT NULL DEFAULT 1,
    created_by TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX guidance_scope ON guidance (scope);

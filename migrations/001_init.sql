-- 001_init.sql — the whole bot schema.
--
-- Conventions:
--   * Every timestamp is RFC 3339 UTC text. Comparisons between two of them
--     happen in Go, never in SQL, so textual ordering never decides anything.
--   * Booleans are INTEGER 0/1.
--   * Ownership is encoded in the queries, not in the schema: every session
--     lookup the bot performs carries "AND owner_user_id = ?". A session id is
--     not a capability.

-- Bot-owned Codex sessions. One row per /new, owned by exactly one Telegram user.
CREATE TABLE IF NOT EXISTS sessions (
    -- Short bot session id, e.g. "s7k3qm". Deliberately NOT a UUID: it can
    -- never be confused with the Codex thread id it points at.
    id              TEXT    PRIMARY KEY,
    owner_user_id   INTEGER NOT NULL,
    -- Human label from /new or /rename. Cosmetic only, never an identity.
    name            TEXT    NOT NULL DEFAULT '',
    -- Absolute workspace this session runs Codex in. Recorded per session so a
    -- later BOT_WORKSPACE change cannot silently resume a thread elsewhere.
    workspace       TEXT    NOT NULL,
    -- Exact thread_id from Codex's thread.started event. NULL until the first
    -- turn of the session produces one.
    codex_thread_id TEXT,
    archived        INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT    NOT NULL,
    updated_at      TEXT    NOT NULL,
    last_turn_at    TEXT
);

CREATE INDEX IF NOT EXISTS idx_sessions_owner
    ON sessions (owner_user_id, archived, updated_at);

-- The selected session per (chat, topic, user). A private chat with topics
-- enabled gets one independent selection per topic; thread_id is 0 when the
-- chat has no topics.
CREATE TABLE IF NOT EXISTS selections (
    chat_id     INTEGER NOT NULL,
    thread_id   INTEGER NOT NULL DEFAULT 0,
    user_id     INTEGER NOT NULL,
    session_id  TEXT    NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    updated_at  TEXT    NOT NULL,
    PRIMARY KEY (chat_id, thread_id, user_id)
);

-- Every Telegram update this process has taken responsibility for. This is what
-- makes "at most one Codex turn per Telegram message" survive a restart: the row
-- is committed before Codex is started, so a redelivered update is recognised.
CREATE TABLE IF NOT EXISTS processed_updates (
    update_id   INTEGER PRIMARY KEY,
    user_id     INTEGER NOT NULL,
    chat_id     INTEGER NOT NULL,
    -- "command", "text", "unsupported" or "dropped" (unauthorized/non-private).
    kind        TEXT    NOT NULL,
    created_at  TEXT    NOT NULL
);

-- One row per Codex turn. update_id is UNIQUE so a single Telegram message can
-- never own two turns, and so a redelivered update can find its turn again.
CREATE TABLE IF NOT EXISTS turns (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    update_id       INTEGER UNIQUE,
    session_id      TEXT    NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    owner_user_id   INTEGER NOT NULL,
    -- running | completed | failed | cancelled | timeout | busy | interrupted
    status          TEXT    NOT NULL,
    codex_thread_id TEXT,
    -- Prompt length in runes. The prompt itself is never stored.
    prompt_chars    INTEGER NOT NULL DEFAULT 0,
    -- Final agent message, kept so a redelivered update whose reply was never
    -- delivered can be answered from the database instead of re-running Codex.
    reply           TEXT,
    -- 1 once Telegram has accepted every chunk of the reply.
    delivered       INTEGER NOT NULL DEFAULT 0,
    -- Sanitized, user-facing error text. Never raw stderr, never argv.
    error           TEXT,
    exit_code       INTEGER,
    -- Exact Codex argv as a JSON array with the prompt elided to "<prompt>".
    argv            TEXT,
    started_at      TEXT    NOT NULL,
    finished_at     TEXT
);

CREATE INDEX IF NOT EXISTS idx_turns_session
    ON turns (session_id, started_at);

CREATE INDEX IF NOT EXISTS idx_turns_status
    ON turns (status) WHERE status = 'running';

-- Small key/value bag: the Telegram update offset and anything similar.
CREATE TABLE IF NOT EXISTS meta (
    key         TEXT PRIMARY KEY,
    value       TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

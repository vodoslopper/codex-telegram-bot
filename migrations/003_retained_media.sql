-- Inbound files stay available to later turns in the same bot session.
-- The bot removes a file after three completed turns judged unrelated to it.
CREATE TABLE IF NOT EXISTS retained_media (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id      TEXT    NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    owner_user_id   INTEGER NOT NULL,
    path            TEXT    NOT NULL UNIQUE,
    kind            TEXT    NOT NULL,
    unrelated_turns INTEGER NOT NULL DEFAULT 0,
    created_at      TEXT    NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_retained_media_session
    ON retained_media (session_id, owner_user_id);

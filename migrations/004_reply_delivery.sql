-- New claims retain the exact private-chat topic for recovery after the
-- Telegram update offset has already advanced. NULL marks pre-migration rows;
-- recovery infers their topic conservatively or uses the recorded private chat.
ALTER TABLE processed_updates ADD COLUMN thread_id INTEGER;

CREATE INDEX IF NOT EXISTS idx_turns_undelivered
    ON turns (id) WHERE status = 'completed' AND delivered = 0;

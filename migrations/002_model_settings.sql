CREATE TABLE model_settings (
    user_id   INTEGER NOT NULL,
    chat_id   INTEGER NOT NULL,
    thread_id INTEGER NOT NULL,
    model     TEXT NOT NULL CHECK (model IN ('gpt-6-luna', 'gpt-6-sol')),
    PRIMARY KEY (user_id, chat_id, thread_id)
);

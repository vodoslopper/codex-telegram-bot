package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ModelSetting returns an explicit model choice, or empty when this scope inherits.
// Chat and thread 0 represent the user's default choice.
func (s *Store) ModelSetting(ctx context.Context, userID, chatID, threadID int64) (string, error) {
	var model string
	err := s.db.QueryRowContext(ctx, `SELECT model FROM model_settings WHERE user_id = ? AND chat_id = ? AND thread_id = ?`,
		userID, chatID, threadID).Scan(&model)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: get model setting: %w", err)
	}
	return model, nil
}

// SetModelSetting saves or clears one explicit choice.
func (s *Store) SetModelSetting(ctx context.Context, userID, chatID, threadID int64, model string) error {
	if userID <= 0 || chatID < 0 || threadID < 0 {
		return errors.New("store: invalid model setting scope")
	}
	if model != "" && model != "gpt-6-luna" && model != "gpt-6-sol" {
		return errors.New("store: unsupported model")
	}
	if model == "" {
		_, err := s.db.ExecContext(ctx, `DELETE FROM model_settings WHERE user_id = ? AND chat_id = ? AND thread_id = ?`, userID, chatID, threadID)
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO model_settings (user_id, chat_id, thread_id, model) VALUES (?, ?, ?, ?)
        ON CONFLICT(user_id, chat_id, thread_id) DO UPDATE SET model = excluded.model`, userID, chatID, threadID, model)
	return err
}

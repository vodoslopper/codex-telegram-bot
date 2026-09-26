package store

import (
	"context"
	"fmt"
)

// RetainedMedia is an inbound Telegram file kept for follow-up turns.
type RetainedMedia struct {
	ID             int64
	SessionID      string
	Path           string
	Kind           string
	UnrelatedTurns int
}

func (s *Store) AddRetainedMedia(ctx context.Context, sessionID string, ownerUserID int64, path, kind string) error {
	res, err := s.db.ExecContext(ctx, `INSERT INTO retained_media
		(session_id, owner_user_id, path, kind, created_at)
		SELECT id, owner_user_id, ?, ?, ? FROM sessions
		WHERE id = ? AND owner_user_id = ?`, path, kind, s.timestamp(), sessionID, ownerUserID)
	if err != nil {
		return fmt.Errorf("store: retain media: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("%w: session %q", ErrNotFound, sessionID)
	}
	return nil
}

func (s *Store) RetainedMedia(ctx context.Context, sessionID string, ownerUserID int64) ([]RetainedMedia, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, session_id, path, kind, unrelated_turns
		FROM retained_media WHERE session_id = ? AND owner_user_id = ? ORDER BY id`, sessionID, ownerUserID)
	if err != nil {
		return nil, fmt.Errorf("store: list retained media: %w", err)
	}
	defer rows.Close()
	var out []RetainedMedia
	for rows.Next() {
		var m RetainedMedia
		if err := rows.Scan(&m.ID, &m.SessionID, &m.Path, &m.Kind, &m.UnrelatedTurns); err != nil {
			return nil, fmt.Errorf("store: scan retained media: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// IncrementMediaUnrelated counts one completed unrelated turn. On the third,
// the row remains until the caller has removed the file, allowing retry after
// a crash or filesystem error.
func (s *Store) IncrementMediaUnrelated(ctx context.Context, id, ownerUserID int64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("store: begin media update: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE retained_media SET unrelated_turns = unrelated_turns + 1
		WHERE id = ? AND owner_user_id = ?`, id, ownerUserID)
	if err != nil {
		return false, fmt.Errorf("store: count unrelated turn: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return false, fmt.Errorf("%w: retained media %d", ErrNotFound, id)
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT unrelated_turns FROM retained_media WHERE id = ?`, id).Scan(&count); err != nil {
		return false, fmt.Errorf("store: read unrelated count: %w", err)
	}
	expired := count >= 3
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("store: commit media update: %w", err)
	}
	return expired, nil
}

func (s *Store) DeleteRetainedMedia(ctx context.Context, id, ownerUserID int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM retained_media WHERE id = ? AND owner_user_id = ?`, id, ownerUserID)
	if err != nil {
		return fmt.Errorf("store: delete retained media: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("%w: retained media %d", ErrNotFound, id)
	}
	return nil
}

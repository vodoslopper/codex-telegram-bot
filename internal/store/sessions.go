package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"codex-telegram-bot/internal/sessid"
)

// Session is one bot-owned Codex session.
type Session struct {
	// ID is the short bot session id ("s7k3qm"), not the Codex thread id.
	ID          string
	OwnerUserID int64
	Name        string
	Workspace   string
	// CodexThreadID is the exact UUID from thread.started. Empty until the
	// session's first turn produces one.
	CodexThreadID string
	Archived      bool
	CreatedAt     time.Time
	UpdatedAt     time.Time
	LastTurnAt    time.Time // zero when no turn has run
}

// HasThread reports whether this session can be resumed.
func (s Session) HasThread() bool { return s.CodexThreadID != "" }

// Label is what the bot shows: the name if one was given, otherwise the id.
func (s Session) Label() string {
	if strings.TrimSpace(s.Name) == "" {
		return s.ID
	}
	return s.Name
}

// maxCreateRetries bounds the (astronomically unlikely) session id collision loop.
const maxCreateRetries = 32

// CreateSession mints a new bot session id and inserts the row.
//
// The id comes from internal/sessid, so it can never be confused with a Codex
// thread UUID, and the retry loop covers a collision with an existing row.
func (s *Store) CreateSession(ctx context.Context, ownerUserID int64, name, workspace string) (Session, error) {
	if ownerUserID <= 0 {
		return Session{}, errors.New("store: a session needs an owner")
	}
	if workspace == "" {
		return Session{}, errors.New("store: a session needs a workspace")
	}
	now := s.timestamp()
	for attempt := 0; attempt < maxCreateRetries; attempt++ {
		id := sessid.New()
		sess := Session{
			ID:          id,
			OwnerUserID: ownerUserID,
			Name:        strings.TrimSpace(name),
			Workspace:   workspace,
			CreatedAt:   s.now(),
			UpdatedAt:   s.now(),
		}
		res, err := s.db.ExecContext(ctx,
			`INSERT INTO sessions (id, owner_user_id, name, workspace, codex_thread_id, archived, created_at, updated_at)
			 VALUES (?, ?, ?, ?, NULL, 0, ?, ?)`,
			sess.ID, sess.OwnerUserID, sess.Name, sess.Workspace, now, now)
		if err != nil {
			if isUniqueViolation(err) {
				continue // id collision: try another
			}
			return Session{}, fmt.Errorf("store: create session: %w", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return Session{}, fmt.Errorf("store: create session affected %d rows", n)
		}
		return sess, nil
	}
	return Session{}, fmt.Errorf("store: could not allocate a unique session id after %d attempts", maxCreateRetries)
}

// sessionColumns is the shared select list, so scanning stays in one place.
const sessionColumns = `id, owner_user_id, name, workspace,
	COALESCE(codex_thread_id, ''), archived, created_at, updated_at, COALESCE(last_turn_at, '')`

func scanSession(row interface{ Scan(...any) error }) (Session, error) {
	var (
		sess       Session
		archived   int
		created    string
		updated    string
		lastTurnAt string
	)
	err := row.Scan(&sess.ID, &sess.OwnerUserID, &sess.Name, &sess.Workspace,
		&sess.CodexThreadID, &archived, &created, &updated, &lastTurnAt)
	if err != nil {
		return Session{}, err
	}
	sess.Archived = archived != 0
	if sess.CreatedAt, err = parseTime(created); err != nil {
		return Session{}, fmt.Errorf("store: bad created_at %q: %w", created, err)
	}
	if sess.UpdatedAt, err = parseTime(updated); err != nil {
		return Session{}, fmt.Errorf("store: bad updated_at %q: %w", updated, err)
	}
	if sess.LastTurnAt, err = parseTime(lastTurnAt); err != nil {
		return Session{}, fmt.Errorf("store: bad last_turn_at %q: %w", lastTurnAt, err)
	}
	return sess, nil
}

// GetSession loads a session and checks ownership in the same statement.
//
// A session that exists but belongs to another user returns ErrNotFound, exactly
// like one that does not exist. Telling those apart would let any allowlisted
// user enumerate the others' sessions.
func (s *Store) GetSession(ctx context.Context, id string, ownerUserID int64) (Session, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+sessionColumns+` FROM sessions WHERE id = ? AND owner_user_id = ?`, id, ownerUserID)
	sess, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, fmt.Errorf("%w: session %q", ErrNotFound, id)
	}
	if err != nil {
		return Session{}, fmt.Errorf("store: get session %q: %w", id, err)
	}
	return sess, nil
}

// ListSessions returns a user's sessions, newest activity first.
//
// Archived sessions are excluded unless includeArchived is set. Sorting happens
// in Go rather than in SQL: RFC 3339 timestamps with a trimmed fractional part
// do not sort correctly as text, and the row count is a handful.
func (s *Store) ListSessions(ctx context.Context, ownerUserID int64, includeArchived bool) ([]Session, error) {
	q := `SELECT ` + sessionColumns + ` FROM sessions WHERE owner_user_id = ?`
	args := []any{ownerUserID}
	if !includeArchived {
		q += ` AND archived = 0`
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list sessions: %w", err)
	}
	defer rows.Close()

	out := make([]Session, 0, 8)
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list sessions: %w", err)
		}
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list sessions: %w", err)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if !a.LastTurnAt.Equal(b.LastTurnAt) {
			return a.LastTurnAt.After(b.LastTurnAt)
		}
		if !a.UpdatedAt.Equal(b.UpdatedAt) {
			return a.UpdatedAt.After(b.UpdatedAt)
		}
		return a.CreatedAt.After(b.CreatedAt)
	})
	return out, nil
}

// CountSessions returns how many sessions a user owns, archived or not.
func (s *Store) CountSessions(ctx context.Context, ownerUserID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE owner_user_id = ?`, ownerUserID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("store: count sessions: %w", err)
	}
	return n, nil
}

// RenameSession sets a session's label. Ownership is part of the WHERE clause.
func (s *Store) RenameSession(ctx context.Context, id string, ownerUserID int64, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("store: a session name must not be empty")
	}
	if len(name) > 120 {
		return errors.New("store: a session name must be at most 120 characters")
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET name = ?, updated_at = ? WHERE id = ? AND owner_user_id = ?`,
		name, s.timestamp(), id, ownerUserID)
	if err != nil {
		return fmt.Errorf("store: rename session %q: %w", id, err)
	}
	return expectOneRow(res, fmt.Sprintf("session %q", id))
}

// SetArchived hides or un-hides a session without touching Codex history. An
// archived session is also deselected in every chat and topic, so the next
// prompt cannot silently continue a session hidden from /sessions.
func (s *Store) SetArchived(ctx context.Context, id string, ownerUserID int64, archived bool) error {
	v := 0
	if archived {
		v = 1
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: archive session %q: %w", id, err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`UPDATE sessions SET archived = ?, updated_at = ? WHERE id = ? AND owner_user_id = ?`,
		v, s.timestamp(), id, ownerUserID)
	if err != nil {
		return fmt.Errorf("store: archive session %q: %w", id, err)
	}
	if err := expectOneRow(res, fmt.Sprintf("session %q", id)); err != nil {
		return err
	}
	if archived {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM selections WHERE session_id = ? AND user_id = ?`, id, ownerUserID); err != nil {
			return fmt.Errorf("store: deselect archived session %q: %w", id, err)
		}
	}
	return tx.Commit()
}

// SetThreadID records the Codex thread a session points at.
//
// It is called after every turn that reported thread.started, not just the first
// one. If a future Codex version forks a new thread id when resuming, the stored
// id follows it and the chain stays intact; if it reuses the id, this is a no-op
// write of the same value. Not updating would be the unsafe choice.
func (s *Store) SetThreadID(ctx context.Context, id string, ownerUserID int64, threadID string) error {
	if !isUUIDString(threadID) {
		return fmt.Errorf("store: refusing to store %q as a thread id: not a UUID", threadID)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET codex_thread_id = ?, updated_at = ? WHERE id = ? AND owner_user_id = ?`,
		threadID, s.timestamp(), id, ownerUserID)
	if err != nil {
		return fmt.Errorf("store: set thread id for %q: %w", id, err)
	}
	return expectOneRow(res, fmt.Sprintf("session %q", id))
}

// TouchLastTurn records that a turn ran on this session.
func (s *Store) TouchLastTurn(ctx context.Context, id string, ownerUserID int64, at time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET last_turn_at = ?, updated_at = ? WHERE id = ? AND owner_user_id = ?`,
		rfc3339(at), s.timestamp(), id, ownerUserID)
	if err != nil {
		return fmt.Errorf("store: touch session %q: %w", id, err)
	}
	return expectOneRow(res, fmt.Sprintf("session %q", id))
}

// --- selections ------------------------------------------------------------

// SelectSession points a (chat, topic, user) triple at one of that user's
// sessions.
//
// The ownership check is inside the INSERT ... SELECT, so there is no window in
// which a session could be selected by somebody who does not own it, and no
// separate read that could race with it. When the SELECT matches nothing — a
// session that does not exist, or one belonging to somebody else — no row is
// written and RowsAffected is 0, which is reported as ErrNotFound.
func (s *Store) SelectSession(ctx context.Context, chatID, threadID, userID int64, sessionID string) error {
	now := s.timestamp()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO selections (chat_id, thread_id, user_id, session_id, updated_at)
		 SELECT ?, ?, ?, s.id, ? FROM sessions s WHERE s.id = ? AND s.owner_user_id = ?
		 ON CONFLICT (chat_id, thread_id, user_id)
		 DO UPDATE SET session_id = excluded.session_id, updated_at = excluded.updated_at`,
		chatID, threadID, userID, now, sessionID, userID)
	if err != nil {
		return fmt.Errorf("store: select session %q: %w", sessionID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: select session %q: %w", sessionID, err)
	}
	if n != 1 {
		// No matching (id, owner) pair: not yours, or not there.
		return fmt.Errorf("%w: session %q", ErrNotFound, sessionID)
	}
	return nil
}

// GetSelection returns the selected session id for a scope, or ErrNotFound.
func (s *Store) GetSelection(ctx context.Context, chatID, threadID, userID int64) (string, error) {
	var sessionID string
	err := s.db.QueryRowContext(ctx,
		`SELECT session_id FROM selections WHERE chat_id = ? AND thread_id = ? AND user_id = ?`,
		chatID, threadID, userID).Scan(&sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: no session selected", ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("store: get selection: %w", err)
	}
	return sessionID, nil
}

// SelectedSession resolves a scope's selection to a session, re-checking
// ownership on the way. A selection whose session row vanished (a restore from
// a partial backup, say) reports ErrNotFound rather than a dangling reference.
func (s *Store) SelectedSession(ctx context.Context, chatID, threadID, userID int64) (Session, error) {
	id, err := s.GetSelection(ctx, chatID, threadID, userID)
	if err != nil {
		return Session{}, err
	}
	return s.GetSession(ctx, id, userID)
}

// ClearSelection drops a scope's selection.
func (s *Store) ClearSelection(ctx context.Context, chatID, threadID, userID int64) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM selections WHERE chat_id = ? AND thread_id = ? AND user_id = ?`,
		chatID, threadID, userID)
	if err != nil {
		return fmt.Errorf("store: clear selection: %w", err)
	}
	return nil
}

// --- helpers ---------------------------------------------------------------

func expectOneRow(res sql.Result, what string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: rows affected for %s: %w", what, err)
	}
	if n != 1 {
		return fmt.Errorf("%w: %s", ErrNotFound, what)
	}
	return nil
}

// isUniqueViolation recognises SQLite's UNIQUE constraint failure, which the
// pure-Go driver reports in the error text.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint") || strings.Contains(msg, "constraint failed")
}

// isUUIDString is the store's own shape check. It duplicates a little of
// internal/codexcli on purpose: the store is the last line of defence before a
// value is handed back to the Codex command line, and it should not have to
// import the runner to refuse a thread *name* where a UUID belongs.
func isUUIDString(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			digit := c >= '0' && c <= '9'
			lower := c >= 'a' && c <= 'f'
			upper := c >= 'A' && c <= 'F'
			if !digit && !lower && !upper {
				return false
			}
		}
	}
	return true
}

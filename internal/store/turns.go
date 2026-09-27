package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// TurnStatus values.
const (
	// TurnRunning means a Codex process is alive for this turn right now.
	TurnRunning = "running"
	// TurnCompleted means turn.completed arrived and the reply was captured.
	TurnCompleted = "completed"
	// TurnFailed means Codex reported an error or exited nonzero.
	TurnFailed = "failed"
	// TurnCancelled means the user sent /stop.
	TurnCancelled = "cancelled"
	// TurnTimeout means BOT_TURN_TIMEOUT elapsed and the group was killed.
	TurnTimeout = "timeout"
	// TurnBusy means the turn was refused because the worktree was in use.
	TurnBusy = "busy"
	// TurnInterrupted means the bot process died while the turn was running.
	// Set at startup; it is a statement about the bot, not about Codex.
	TurnInterrupted = "interrupted"
)

// Terminal reports whether a status will not change again on its own.
func Terminal(status string) bool {
	switch status {
	case TurnCompleted, TurnFailed, TurnCancelled, TurnTimeout, TurnBusy, TurnInterrupted:
		return true
	default:
		return false
	}
}

// metaKeyOffset is the meta row holding the Telegram update offset.
const metaKeyOffset = "telegram_update_offset"

// Turn is one Codex invocation on behalf of one Telegram message.
type Turn struct {
	ID            int64
	UpdateID      int64 // 0 when the row has none
	SessionID     string
	OwnerUserID   int64
	Status        string
	CodexThreadID string
	PromptChars   int
	Reply         string
	Delivered     bool
	Error         string
	ExitCode      int
	Argv          []string
	StartedAt     time.Time
	FinishedAt    time.Time
}

// ClaimUpdate records that this process has taken responsibility for an update.
//
// It returns false when the update was already claimed, which is how a
// redelivered update is recognised after a crash between claiming it and
// advancing the offset. The insert is committed by the caller's context before
// Codex is started; that ordering is the whole deduplication guarantee.
func (s *Store) ClaimUpdate(ctx context.Context, updateID, userID, chatID int64, kind string) (bool, error) {
	return s.ClaimUpdateInThread(ctx, updateID, userID, chatID, 0, kind)
}

// ClaimUpdateInThread also records the exact topic needed to retry a reply
// after the update offset has advanced.
func (s *Store) ClaimUpdateInThread(ctx context.Context, updateID, userID, chatID, threadID int64, kind string) (bool, error) {
	if updateID <= 0 {
		return false, fmt.Errorf("store: invalid update id %d", updateID)
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO processed_updates (update_id, user_id, chat_id, thread_id, kind, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		updateID, userID, chatID, threadID, kind, s.timestamp())
	if err != nil {
		return false, fmt.Errorf("store: claim update %d: %w", updateID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: claim update %d: %w", updateID, err)
	}
	return n == 1, nil
}

// PendingReply is a completed answer that Telegram has not confirmed. Its
// scope comes from the original update, so topic replies stay in that topic.
type PendingReply struct {
	TurnID     int64
	UpdateID   int64
	SessionID  string
	ChatID     int64
	ThreadID   int64
	UserID     int64
	Legacy     bool
	TopicKnown bool
	Reply      string
	StartedAt  time.Time
	FinishedAt time.Time
}

// PendingReplies returns a bounded batch for automatic delivery retry. For an
// older claim without a topic id, a unique selection that predates the turn can
// identify the original topic. Otherwise the caller uses the private chat root.
func (s *Store) PendingReplies(ctx context.Context, afterID int64, limit int) ([]PendingReply, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT t.id, t.update_id, t.session_id, p.chat_id, p.thread_id,
		p.user_id, t.reply, t.started_at, t.finished_at
		FROM turns t JOIN processed_updates p ON p.update_id = t.update_id
		WHERE t.status = ? AND t.delivered = 0 AND t.reply IS NOT NULL
		ORDER BY (t.id <= ?), t.id LIMIT ?`, TurnCompleted, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list pending replies: %w", err)
	}
	var out []PendingReply
	for rows.Next() {
		var p PendingReply
		var threadID sql.NullInt64
		var started, finished string
		if err := rows.Scan(&p.TurnID, &p.UpdateID, &p.SessionID, &p.ChatID, &threadID,
			&p.UserID, &p.Reply, &started, &finished); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: scan pending reply: %w", err)
		}
		p.Legacy = !threadID.Valid
		p.TopicKnown = threadID.Valid
		if threadID.Valid {
			p.ThreadID = threadID.Int64
		}
		if p.StartedAt, err = parseTime(started); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: pending reply start time: %w", err)
		}
		if p.FinishedAt, err = parseTime(finished); err != nil {
			rows.Close()
			return nil, fmt.Errorf("store: pending reply finish time: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("store: read pending replies: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("store: close pending replies: %w", err)
	}
	for i := range out {
		if !out[i].Legacy {
			continue
		}
		threadID, known, err := s.inferLegacyTopic(ctx, out[i])
		if err != nil {
			return nil, err
		}
		out[i].ThreadID, out[i].TopicKnown = threadID, known
	}
	return out, nil
}

func (s *Store) inferLegacyTopic(ctx context.Context, reply PendingReply) (int64, bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT thread_id, updated_at FROM selections
		WHERE chat_id = ? AND user_id = ? AND session_id = ?`,
		reply.ChatID, reply.UserID, reply.SessionID)
	if err != nil {
		return 0, false, fmt.Errorf("store: find legacy reply topic: %w", err)
	}
	defer rows.Close()
	var candidate int64
	var matches int
	for rows.Next() {
		var threadID int64
		var updated string
		if err := rows.Scan(&threadID, &updated); err != nil {
			return 0, false, fmt.Errorf("store: scan legacy topic: %w", err)
		}
		selectedAt, err := parseTime(updated)
		if err != nil {
			return 0, false, fmt.Errorf("store: parse legacy selection time: %w", err)
		}
		if selectedAt.After(reply.StartedAt) {
			continue
		}
		candidate = threadID
		matches++
	}
	if err := rows.Err(); err != nil {
		return 0, false, fmt.Errorf("store: read legacy topics: %w", err)
	}
	return candidate, matches == 1, nil
}

// UpdateClaimed reports whether an update id is already in the ledger, without
// claiming it.
func (s *Store) UpdateClaimed(ctx context.Context, updateID int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM processed_updates WHERE update_id = ?`, updateID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store: check update %d: %w", updateID, err)
	}
	return n > 0, nil
}

// BeginTurn inserts a running turn row and returns its id.
//
// turns.update_id is UNIQUE, so this is a second, independent guard against one
// Telegram message producing two Codex runs: even if a caller forgot to claim
// the update first, the insert fails and no process is started.
func (s *Store) BeginTurn(ctx context.Context, updateID int64, sessionID string, ownerUserID int64,
	promptChars int, argv []string) (int64, error) {

	encoded, err := json.Marshal(argv)
	if err != nil {
		encoded = []byte("[]")
	}
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO turns (update_id, session_id, owner_user_id, status, prompt_chars, argv, started_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		nullIfZero(updateID), sessionID, ownerUserID, TurnRunning, promptChars, string(encoded), s.timestamp())
	if err != nil {
		if isUniqueViolation(err) {
			return 0, fmt.Errorf("%w: update %d already has a turn", ErrAlreadyClaimed, updateID)
		}
		return 0, fmt.Errorf("store: begin turn: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("store: begin turn: last insert id: %w", err)
	}
	return id, nil
}

// UpdateTurnArgv records the final invocation after workspace-locked attachment
// paths have been resolved. The prompt remains elided by the caller.
func (s *Store) UpdateTurnArgv(ctx context.Context, turnID, ownerUserID int64, argv []string) error {
	encoded, err := json.Marshal(argv)
	if err != nil {
		return fmt.Errorf("store: encode turn argv: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE turns SET argv = ? WHERE id = ? AND owner_user_id = ?`, string(encoded), turnID, ownerUserID)
	if err != nil {
		return fmt.Errorf("store: update turn argv: %w", err)
	}
	return expectOneRow(res, fmt.Sprintf("turn %d", turnID))
}

// FinishTurn records the outcome of a turn.
//
// reply may be empty for a failed turn. exitCode is the child's exit status, or
// -1 when it could not be determined.
func (s *Store) FinishTurn(ctx context.Context, turnID int64, status, threadID, reply, errMsg string, exitCode int) error {
	if !Terminal(status) {
		return fmt.Errorf("store: %q is not a terminal turn status", status)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE turns
		    SET status = ?, codex_thread_id = ?, reply = ?, error = ?, exit_code = ?, finished_at = ?
		  WHERE id = ?`,
		status, nullIfEmpty(threadID), nullIfEmpty(reply), nullIfEmpty(errMsg), exitCode, s.timestamp(), turnID)
	if err != nil {
		return fmt.Errorf("store: finish turn %d: %w", turnID, err)
	}
	return expectOneRow(res, fmt.Sprintf("turn %d", turnID))
}

// MarkTurnDelivered records that Telegram accepted every chunk of the reply.
//
// This flag is what lets a redelivered update answer from the database instead
// of either re-running Codex or staying silent: delivered means "say nothing",
// not delivered means "send the stored reply".
func (s *Store) MarkTurnDelivered(ctx context.Context, turnID int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE turns SET delivered = 1 WHERE id = ?`, turnID)
	if err != nil {
		return fmt.Errorf("store: mark turn %d delivered: %w", turnID, err)
	}
	return expectOneRow(res, fmt.Sprintf("turn %d", turnID))
}

const turnColumns = `id, COALESCE(update_id, 0), session_id, owner_user_id, status,
	COALESCE(codex_thread_id, ''), prompt_chars, COALESCE(reply, ''), delivered,
	COALESCE(error, ''), COALESCE(exit_code, -999), COALESCE(argv, '[]'),
	started_at, COALESCE(finished_at, '')`

func scanTurn(row interface{ Scan(...any) error }) (Turn, error) {
	var (
		t         Turn
		delivered int
		exitCode  int
		argvJSON  string
		started   string
		finished  string
	)
	err := row.Scan(&t.ID, &t.UpdateID, &t.SessionID, &t.OwnerUserID, &t.Status,
		&t.CodexThreadID, &t.PromptChars, &t.Reply, &delivered, &t.Error,
		&exitCode, &argvJSON, &started, &finished)
	if err != nil {
		return Turn{}, err
	}
	t.Delivered = delivered != 0
	if exitCode == -999 {
		exitCode = 0
		t.ExitCode = 0
	} else {
		t.ExitCode = exitCode
	}
	if argvJSON != "" {
		_ = json.Unmarshal([]byte(argvJSON), &t.Argv) // diagnostics only; ignore a bad row
	}
	if t.StartedAt, err = parseTime(started); err != nil {
		return Turn{}, fmt.Errorf("store: bad started_at %q: %w", started, err)
	}
	if t.FinishedAt, err = parseTime(finished); err != nil {
		return Turn{}, fmt.Errorf("store: bad finished_at %q: %w", finished, err)
	}
	return t, nil
}

// TurnByUpdate finds the turn owned by an update id, if any.
func (s *Store) TurnByUpdate(ctx context.Context, updateID int64) (Turn, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+turnColumns+` FROM turns WHERE update_id = ?`, updateID)
	t, err := scanTurn(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Turn{}, fmt.Errorf("%w: no turn for update %d", ErrNotFound, updateID)
	}
	if err != nil {
		return Turn{}, fmt.Errorf("store: turn for update %d: %w", updateID, err)
	}
	return t, nil
}

// MarkInterruptedTurns flips every 'running' turn to 'interrupted'.
//
// It runs once at startup. A 'running' row at that moment can only mean the
// previous process died while Codex was alive, since the row is written by the
// process that owns it. The bot cannot know whether Codex finished its work:
// file edits may have landed, the reply may even have been computed and lost.
// What it can do is make the state visible, which is what /session reports.
func (s *Store) MarkInterruptedTurns(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE turns SET status = ?, error = ?, finished_at = ? WHERE status = ?`,
		TurnInterrupted,
		"the bot restarted while this turn was running; Codex may have made partial changes",
		s.timestamp(), TurnRunning)
	if err != nil {
		return 0, fmt.Errorf("store: mark interrupted turns: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: mark interrupted turns: %w", err)
	}
	return n, nil
}

// LastTurnForSession returns a session's most recent turn.
//
// "Most recent" is decided by the AUTOINCREMENT id rather than by a timestamp,
// because two turns inside the same nanosecond would tie and an id cannot.
func (s *Store) LastTurnForSession(ctx context.Context, sessionID string) (Turn, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+turnColumns+` FROM turns WHERE session_id = ? ORDER BY id DESC LIMIT 1`, sessionID)
	t, err := scanTurn(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Turn{}, fmt.Errorf("%w: no turns for session %q", ErrNotFound, sessionID)
	}
	if err != nil {
		return Turn{}, fmt.Errorf("store: last turn for %q: %w", sessionID, err)
	}
	return t, nil
}

// HasRunningTurn reports whether the database still shows a running turn for a
// session. The in-memory lock is the real guard; this is the belt that catches a
// lock leaked by a bug, and it is what /session displays after a restart.
func (s *Store) HasRunningTurn(ctx context.Context, sessionID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM turns WHERE session_id = ? AND status = ?`, sessionID, TurnRunning).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("store: running turns for %q: %w", sessionID, err)
	}
	return n > 0, nil
}

// --- Telegram update offset ------------------------------------------------

// GetOffset returns the persisted getUpdates offset, or 0 when none is stored.
func (s *Store) GetOffset(ctx context.Context) (int64, error) {
	raw, err := s.GetMeta(ctx, metaKeyOffset)
	if err != nil {
		return 0, err
	}
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("store: stored offset %q is not a number: %w", raw, err)
	}
	return n, nil
}

// AdvanceOffset stores a new offset, but only forwards.
//
// Refusing to move backwards means a bug in the poller cannot replay history and
// re-run old prompts. Rewinding deliberately is an operator action: edit the
// meta row by hand, with the bot stopped.
func (s *Store) AdvanceOffset(ctx context.Context, next int64) error {
	cur, err := s.GetOffset(ctx)
	if err != nil {
		return err
	}
	if next <= cur {
		return nil
	}
	return s.SetMeta(ctx, metaKeyOffset, strconv.FormatInt(next, 10))
}

func nullIfZero(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

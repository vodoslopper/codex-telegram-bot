// Package bot is the orchestration layer: it turns authorized Telegram messages
// into Codex turns and Codex replies back into Telegram messages.
//
// The gates an update passes through, in order, are:
//
//  1. It is a plain message (not an edit, a channel post or a callback).
//  2. The chat is private. Groups and channels are dropped without a reply.
//  3. from.id is on the allowlist. This is checked before any command is parsed,
//     before the database is touched for that update, and before any process is
//     started.
//  4. The update id has not been claimed already.
//
// Only then is a command interpreted or a Codex turn started. An update that
// fails a gate is still claimed, so it is not redelivered forever, and is
// otherwise discarded.
//
// Identity is from.id and nothing else. Usernames are mutable and absent from
// some messages, so they are never consulted for authorization.
package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"codex-telegram-bot/internal/codexcli"
	"codex-telegram-bot/internal/config"
	"codex-telegram-bot/internal/redact"
	"codex-telegram-bot/internal/store"
	"codex-telegram-bot/internal/telegram"
	"codex-telegram-bot/internal/textsplit"
)

// Telegram is the slice of the Bot API this bot uses. Declaring it here rather
// than depending on *telegram.Client keeps the package testable against an
// in-memory fake.
type Telegram interface {
	SendMessage(ctx context.Context, chatID, threadID int64, text string) (*telegram.SentMessage, error)
	SendDocument(ctx context.Context, chatID, threadID int64, path string) (*telegram.SentMessage, error)
	DownloadFile(ctx context.Context, fileID string) ([]byte, error)
	SendChatAction(ctx context.Context, chatID, threadID int64, action string) error
	GetUpdates(ctx context.Context, offset int64, pollTimeout time.Duration, limit int) ([]telegram.Update, error)
	GetMe(ctx context.Context) (*telegram.User, error)
	GetWebhookInfo(ctx context.Context) (*telegram.WebhookInfo, error)
	DeleteWebhook(ctx context.Context, dropPending bool) error
}

// Store is the persistence this package needs. *store.Store satisfies it.
type Store interface {
	ClaimUpdate(ctx context.Context, updateID, userID, chatID int64, kind string) (bool, error)
	TurnByUpdate(ctx context.Context, updateID int64) (store.Turn, error)
	BeginTurn(ctx context.Context, updateID int64, sessionID string, ownerUserID int64, promptChars int, argv []string) (int64, error)
	UpdateTurnArgv(ctx context.Context, turnID, ownerUserID int64, argv []string) error
	FinishTurn(ctx context.Context, turnID int64, status, threadID, reply, errMsg string, exitCode int) error
	MarkTurnDelivered(ctx context.Context, turnID int64) error
	MarkInterruptedTurns(ctx context.Context) (int64, error)
	HasRunningTurn(ctx context.Context, sessionID string) (bool, error)
	LastTurnForSession(ctx context.Context, sessionID string) (store.Turn, error)

	CreateSession(ctx context.Context, ownerUserID int64, name, workspace string) (store.Session, error)
	GetSession(ctx context.Context, id string, ownerUserID int64) (store.Session, error)
	ListSessions(ctx context.Context, ownerUserID int64, includeArchived bool) ([]store.Session, error)
	RenameSession(ctx context.Context, id string, ownerUserID int64, name string) error
	SetArchived(ctx context.Context, id string, ownerUserID int64, archived bool) error
	SetThreadID(ctx context.Context, id string, ownerUserID int64, threadID string) error
	TouchLastTurn(ctx context.Context, id string, ownerUserID int64, at time.Time) error
	SelectSession(ctx context.Context, chatID, threadID, userID int64, sessionID string) error
	SelectedSession(ctx context.Context, chatID, threadID, userID int64) (store.Session, error)
	ModelSetting(ctx context.Context, userID, chatID, threadID int64) (string, error)
	SetModelSetting(ctx context.Context, userID, chatID, threadID int64, model string) error
	AddRetainedMedia(ctx context.Context, sessionID string, ownerUserID int64, path, kind string) error
	RetainedMedia(ctx context.Context, sessionID string, ownerUserID int64) ([]store.RetainedMedia, error)
	IncrementMediaUnrelated(ctx context.Context, id, ownerUserID int64) (bool, error)
	DeleteRetainedMedia(ctx context.Context, id, ownerUserID int64) error

	GetOffset(ctx context.Context) (int64, error)
	AdvanceOffset(ctx context.Context, next int64) error
}

// Codex runs one turn. *codexcli.Runner satisfies it.
type Codex interface {
	Run(ctx context.Context, req codexcli.Request) (*codexcli.Result, error)
	SessionUsage(ctx context.Context, threadID string) (codexcli.UsageSnapshot, error)
	LiveRateLimits(ctx context.Context) (*codexcli.RateLimits, error)
	// ElidedArgv returns the exact argument list a request would produce, with
	// the prompt replaced by a placeholder. It is recorded with the turn so a
	// failed one can be debugged without persisting the user's text.
	ElidedArgv(req codexcli.Request) []string
	Sandbox() string
}

// Bot wires the pieces together.
type Bot struct {
	cfg *config.Config
	tg  Telegram
	st  Store
	cx  Codex
	log *slog.Logger

	// botUsername is filled in by Identify at startup and used to accept
	// "/command@thisbot".
	botUsername string
	// codexVersion is the string from `codex --version`, shown in /start.
	codexVersion string

	scopeLocks *keyedLocks
	wsLocks    *keyedLocks
	inflight   *inflightRegistry

	// pendingTurns counts turns that are queued or running. It bounds how much
	// work the bot takes on, and it is checked without blocking so the poller
	// keeps receiving updates (including /stop) while turns are in flight.
	pendingTurns atomic.Int64

	// work is the context handed to update handlers, so that stopping the
	// poller does not abort a Codex turn that is already editing files.
	workMu sync.RWMutex
	work   context.Context

	now func() time.Time
	wg  sync.WaitGroup
}

// New builds a Bot. It does no I/O.
func New(cfg *config.Config, tg Telegram, st Store, cx Codex, log *slog.Logger) *Bot {
	if log == nil {
		log = slog.Default()
	}
	return &Bot{
		cfg:        cfg,
		tg:         tg,
		st:         st,
		cx:         cx,
		log:        log,
		scopeLocks: newKeyedLocks(),
		wsLocks:    newKeyedLocks(),
		inflight:   newInflightRegistry(),
		now:        func() time.Time { return time.Now().UTC() },
	}
}

// SetCodexVersion records the string from `codex --version`, which /help shows.
//
// The bot's own username is not passed in: Prepare learns it from getMe, which is
// the only authoritative source, and a stale value would make
// "/command@thisbot" stop being recognised.
func (b *Bot) SetCodexVersion(version string) { b.codexVersion = version }

// SetClock replaces the time source; tests use it.
func (b *Bot) SetClock(f func() time.Time) {
	if f != nil {
		b.now = f
	}
}

// setWorkContext records the context update handlers run under.
func (b *Bot) setWorkContext(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	b.workMu.Lock()
	b.work = ctx
	b.workMu.Unlock()
}

// workContext returns the handler context, defaulting to Background when Run has
// not been called — which is the case for a direct HandleUpdate call in a test.
func (b *Bot) workContext() context.Context {
	b.workMu.RLock()
	defer b.workMu.RUnlock()
	if b.work == nil {
		return context.Background()
	}
	return b.work
}

// Wait blocks until every dispatched update handler has returned. Shutdown uses
// it after cancelling the poller context.
func (b *Bot) Wait() { b.wg.Wait() }

// Prepared is an update that cleared every gate.
type Prepared struct {
	UpdateID int64
	// MessageID is Telegram's id for the message, logged so an operator can
	// correlate a turn with what the user actually sent.
	MessageID int64
	Scope     Scope
	// Cmd is non-nil for a command message.
	Cmd *Command
	// Text is the message text, for a plain (non-command) message.
	Text string
	// Media is one supported attachment accompanying the caption.
	Media *mediaAttachment
	// Unsupported is set for an authorized private text message the bot cannot
	// act on (a sticker or an empty message).
	Unsupported bool
	// Duplicate is set when the update id was already claimed by an earlier
	// run of this bot. Nothing is executed for it; an undelivered reply may
	// still be resent.
	Duplicate bool
}

// Claim applies the gates to one update and records it as processed.
//
// It returns nil when the update must be ignored completely (not a message, not
// a private chat, sender not on the allowlist). It returns a Prepared with
// Duplicate set when this update id was already handled, which is the signal to
// resend an undelivered reply rather than to run Codex again.
//
// The database write happens here, synchronously in the poller, before the
// caller advances its offset. That ordering is what makes the deduplication
// guarantee hold across a crash.
func (b *Bot) Claim(ctx context.Context, u telegram.Update) (*Prepared, error) {
	msg := u.Message
	if msg == nil {
		// Claim it anyway so Telegram stops redelivering something this build
		// will never handle.
		_, _ = b.claim(ctx, u.UpdateID, 0, 0, u.Kind())
		return nil, nil
	}

	if !msg.Chat.IsPrivate() {
		b.log.Info("ignoring a message from a non-private chat",
			"chat_type", msg.Chat.Type, "chat_id", msg.Chat.ID, "update_id", u.UpdateID)
		_, _ = b.claim(ctx, u.UpdateID, senderID(msg), msg.Chat.ID, "dropped:not-private")
		return nil, nil
	}
	if msg.From == nil {
		b.log.Warn("ignoring a private message with no sender", "update_id", u.UpdateID)
		_, _ = b.claim(ctx, u.UpdateID, 0, msg.Chat.ID, "dropped:no-sender")
		return nil, nil
	}
	if msg.From.IsBot {
		_, _ = b.claim(ctx, u.UpdateID, msg.From.ID, msg.Chat.ID, "dropped:bot-sender")
		return nil, nil
	}
	if !b.cfg.Allowed(msg.From.ID) {
		// The allowlist is the first gate that involves a decision about a
		// person, so it is worth a log line — with the numeric id only, never
		// the username and never the text.
		b.log.Warn("rejecting a message from a user who is not on the allowlist",
			"user_id", msg.From.ID, "update_id", u.UpdateID)
		_, _ = b.claim(ctx, u.UpdateID, msg.From.ID, msg.Chat.ID, "dropped:not-allowed")
		return nil, nil
	}

	scope := Scope{ChatID: msg.Chat.ID, ThreadID: msg.ThreadID(), UserID: msg.From.ID}
	p := &Prepared{UpdateID: u.UpdateID, MessageID: msg.MessageID, Scope: scope}

	text := msg.Text
	p.Media = attachmentOf(msg)
	switch {
	case p.Media != nil:
		p.Text = msg.Caption
		claimed, err := b.claim(ctx, u.UpdateID, scope.UserID, scope.ChatID, "media:"+p.Media.kind)
		if err != nil {
			return nil, err
		}
		p.Duplicate = !claimed
	case strings.TrimSpace(text) == "":
		p.Unsupported = true
		// The error is propagated here, unlike the drop paths above: this
		// update gets a reply, so a claim that was not recorded would be
		// redelivered and answered twice. A dropped update does not have that
		// problem, and refusing to advance past spam would stall the poller.
		claimed, err := b.claim(ctx, u.UpdateID, scope.UserID, scope.ChatID, "unsupported")
		if err != nil {
			return nil, err
		}
		p.Duplicate = !claimed
	case strings.HasPrefix(strings.TrimSpace(text), "/"):
		p.Cmd = ParseCommand(text, b.botUsername)
		kind := "command"
		if p.Cmd != nil {
			kind = "command:" + p.Cmd.Name
		}
		claimed, err := b.claim(ctx, u.UpdateID, scope.UserID, scope.ChatID, kind)
		if err != nil {
			return nil, err
		}
		p.Duplicate = !claimed
	default:
		p.Text = text
		claimed, err := b.claim(ctx, u.UpdateID, scope.UserID, scope.ChatID, "text")
		if err != nil {
			return nil, err
		}
		p.Duplicate = !claimed
	}
	return p, nil
}

// claim writes the deduplication row. An error here is worth reporting because
// the offset must not advance past an update we failed to record.
func (b *Bot) claim(ctx context.Context, updateID, userID, chatID int64, kind string) (bool, error) {
	claimed, err := b.st.ClaimUpdate(ctx, updateID, userID, chatID, kind)
	if err != nil {
		b.log.Error("could not record an update as processed", "update_id", updateID, "error", err.Error())
		return false, err
	}
	return claimed, nil
}

func senderID(m *telegram.Message) int64 {
	if m == nil || m.From == nil {
		return 0
	}
	return m.From.ID
}

// Handle acts on a prepared update. It never returns an error: anything that
// goes wrong is reported to the user or logged, because there is nobody left to
// handle it and the update has already been claimed.
func (b *Bot) Handle(ctx context.Context, p *Prepared) {
	if p == nil {
		return
	}
	if p.Duplicate {
		b.recoverUndelivered(ctx, p)
		return
	}
	switch {
	case p.Unsupported:
		b.send(ctx, p.Scope, "I handle text, photos, documents, audio and video. Send /help for the commands.")
	case p.Cmd != nil:
		b.handleCommand(ctx, p)
	default:
		b.handleText(ctx, p)
	}
}

// HandleUpdate is Claim and Handle together. The poller splits them so it can
// claim synchronously and handle concurrently; tests and a one-shot run use
// this.
func (b *Bot) HandleUpdate(ctx context.Context, u telegram.Update) {
	p, err := b.Claim(ctx, u)
	if err != nil || p == nil {
		return
	}
	b.Handle(ctx, p)
}

// recoverUndelivered resends a finished reply that Telegram never accepted.
//
// This is the narrow window the design cannot close any further: the process
// died after Codex completed a turn but before (or during) delivery. The turn is
// already recorded, so Codex is never run a second time; the reply is in the
// database, so the user still gets an answer. If the reply had been delivered,
// this stays silent rather than duplicating it.
func (b *Bot) recoverUndelivered(ctx context.Context, p *Prepared) {
	t, err := b.st.TurnByUpdate(ctx, p.UpdateID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			b.log.Warn("could not look up a duplicate update", "update_id", p.UpdateID, "error", err.Error())
		}
		// Not a turn (a command, or an unsupported message): already handled,
		// and repeating a command's answer would be noise.
		b.log.Info("ignoring a duplicate update", "update_id", p.UpdateID)
		return
	}
	if t.Status != store.TurnCompleted || t.Delivered || strings.TrimSpace(t.Reply) == "" {
		b.log.Info("ignoring a duplicate update",
			"update_id", p.UpdateID, "turn_status", t.Status, "delivered", t.Delivered)
		return
	}
	b.log.Warn("redelivering a reply whose delivery was never confirmed",
		"update_id", p.UpdateID, "turn_id", t.ID, "session_id", t.SessionID)
	if err := b.deliverReply(ctx, p.Scope, t.Reply, t.StartedAt); err != nil {
		b.log.Error("redelivery failed", "update_id", p.UpdateID, "error", err.Error())
		return
	}
	if err := b.st.MarkTurnDelivered(ctx, t.ID); err != nil {
		b.log.Error("could not mark a redelivered turn", "turn_id", t.ID, "error", err.Error())
	}
}

// --- outbound messages -----------------------------------------------------

// send delivers text to a scope, split into Telegram-sized chunks.
//
// Chunks are sent in order and a failure stops the sequence: a half-delivered
// reply is better than a reordered one, and the caller uses the error to decide
// whether the turn may be marked delivered.
func (b *Bot) send(ctx context.Context, scope Scope, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	chunks := textsplit.Split(text, b.cfg.MaxReplyChars)
	for i, chunk := range chunks {
		if chunk == "" {
			continue
		}
		if _, err := b.tg.SendMessage(ctx, scope.ChatID, scope.ThreadID, chunk); err != nil {
			if telegram.IsForbidden(err) || telegram.IsNotFound(err) {
				// The user blocked the bot or deleted the chat. Nothing to
				// retry, and worth a log line so the operator understands the
				// silence.
				b.log.Warn("could not deliver a message; the user may have blocked the bot",
					"chat_id", scope.ChatID, "chunk", i+1, "of", len(chunks),
					"error", err.Error())
			} else {
				b.log.Error("could not deliver a message",
					"chat_id", scope.ChatID, "chunk", i+1, "of", len(chunks),
					"error", err.Error())
			}
			return fmt.Errorf("send chunk %d/%d: %w", i+1, len(chunks), err)
		}
	}
	return nil
}

// sendBest is send with the error logged and dropped, for cosmetic messages
// where a failure must not change the outcome of a turn.
func (b *Bot) sendBest(ctx context.Context, scope Scope, text string) {
	if err := b.send(ctx, scope, text); err != nil {
		b.log.Debug("a best-effort message failed", "chat_id", scope.ChatID, "error", err.Error())
	}
}

// typing keeps the "typing…" indicator alive while ctx lives. Telegram shows it
// for about five seconds, so it is refreshed on an interval.
func (b *Bot) typing(ctx context.Context, scope Scope) {
	if b.cfg.TypingInterval <= 0 {
		return
	}
	// Cosmetic: an error here is not worth more than a debug line.
	send := func() {
		if err := b.tg.SendChatAction(ctx, scope.ChatID, scope.ThreadID, "typing"); err != nil {
			b.log.Debug("typing indicator failed", "error", err.Error())
		}
	}
	// Once immediately, so even a turn that finishes in a second shows the
	// indicator, then on an interval for as long as it runs.
	send()
	ticker := time.NewTicker(b.cfg.TypingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			send()
		}
	}
}

// logPrompt writes a prompt to the log only when the operator asked for it.
//
// By default the log carries the length and nothing else. A message to a coding
// agent can contain anything the operator would not want in a journal that is
// world-readable on a shared host, so the content is opt-in.
func (b *Bot) logPrompt(level slog.Level, msg string, scope Scope, text string) {
	if !b.cfg.LogPrompts {
		b.log.Log(context.Background(), level, msg,
			"chat_id", scope.ChatID, "thread_id", scope.ThreadID,
			"user_id", scope.UserID, "prompt_chars", len([]rune(text)))
		return
	}
	b.log.Log(context.Background(), level, msg,
		"chat_id", scope.ChatID, "thread_id", scope.ThreadID,
		"user_id", scope.UserID, "prompt_chars", len([]rune(text)),
		"prompt", redact.Preview(text, 400))
}

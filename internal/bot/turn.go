package bot

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"codex-telegram-bot/internal/codexcli"
	"codex-telegram-bot/internal/redact"
	"codex-telegram-bot/internal/store"
)

// handleText deals with an ordinary message: it becomes a Codex prompt.
//
// Admission control happens here rather than in the poller. The poller must keep
// polling no matter how many turns are in flight, because /stop arrives as an
// update too and a blocked poller could not receive it. So the count is checked
// without blocking and an over-limit message gets a clear refusal instead of a
// place in an unbounded queue.
func (b *Bot) handleText(ctx context.Context, p *Prepared) {
	if p.busy || (!p.admitted && !b.reserveTurn()) {
		b.log.Warn("refusing a turn: too many are already queued or running",
			"pending", b.pendingTurns.Load(), "limit", b.cfg.MaxConcurrent, "user_id", p.Scope.UserID)
		b.sendBest(ctx, p.Scope, fmt.Sprintf(
			"I already have %d turn(s) queued or running, which is my limit. "+
				"Wait for them to finish, or send /stop to cancel the one in this chat.", b.cfg.MaxConcurrent))
		return
	}
	if !p.admitted {
		defer b.pendingTurns.Add(-1)
	}
	b.runTurn(ctx, p)
}

// reserveTurn makes the configured cap an atomic admission decision. The poller
// calls it before dispatch, so turns waiting for their scope are counted too.
func (b *Bot) reserveTurn() bool {
	limit := int64(b.cfg.MaxConcurrent)
	for {
		current := b.pendingTurns.Load()
		if limit > 0 && current >= limit {
			return false
		}
		if b.pendingTurns.CompareAndSwap(current, current+1) {
			return true
		}
	}
}

// turnOutcome is what one Codex run produced, in the form the database and the
// user both need.
type turnOutcome struct {
	status    string
	reply     string
	errMsg    string
	res       *codexcli.Result
	threadID  string
	exitCode  int
	startedAt time.Time
}

// runTurn executes one Codex turn for a message and reports the outcome.
//
// Locking order, always the same so nothing can deadlock:
//
//	scope lock  →  workspace lock  →  run Codex
//
// The scope lock serialises the turns of one chat/topic, which is what keeps two
// quick messages from the same person from interleaving. The workspace lock
// serialises turns that share a working tree, because two agents editing one
// checkout at once corrupt each other's work. With a single BOT_WORKSPACE the
// second lock effectively makes the whole bot one turn at a time, which is the
// point: Codex edits files.
func (b *Bot) runTurn(ctx context.Context, p *Prepared) {
	prompt := p.Text
	b.logPrompt(slog.LevelInfo, "starting a turn", p.Scope, prompt)

	// A cancellable context for the whole turn, registered before any waiting so
	// /stop can reach a turn that is still queued for a lock.
	turnCtx, entry := p.turnCtx, p.entry
	if turnCtx == nil {
		var cancelTurn context.CancelFunc
		turnCtx, cancelTurn = context.WithCancel(ctx)
		defer cancelTurn()
		entry = &inflightTurn{cancel: cancelTurn, started: b.now()}
		b.inflight.register(p.Scope, entry)
		defer b.inflight.clear(p.Scope, entry)
	}

	// The typing indicator covers queueing as well as the turn itself, so a long
	// wait does not look like a dead bot.
	typingCtx, stopTyping := context.WithCancel(turnCtx)
	defer stopTyping()
	go b.typing(typingCtx, p.Scope)

	// --- scope lock ---------------------------------------------------------
	waitCtx, cancelWait := context.WithTimeout(turnCtx, b.cfg.QueueTimeout)
	defer cancelWait()

	releaseScope, err := b.scopeLocks.Acquire(waitCtx, scopeKey(p.Scope))
	if err != nil {
		b.reportQueueFailure(turnCtx, p, "another turn in this chat", err)
		return
	}
	defer releaseScope()

	// --- session ------------------------------------------------------------
	sess, notice, err := b.ensureSession(turnCtx, p)
	if err != nil {
		b.log.Error("could not resolve a session for a message",
			"user_id", p.Scope.UserID, "error", err.Error())
		b.sendBest(turnCtx, p.Scope,
			"I could not set up a Codex session for this message: "+firstLine(err.Error()))
		return
	}
	entry.setSession(sess.ID)
	if notice != "" {
		b.sendBest(turnCtx, p.Scope, notice)
	}
	var imagePaths []string
	var available []store.RetainedMedia
	var mediaPath string
	if p.Media != nil {
		path, err := plannedMediaPath(b.cfg.Workspace, p.Media.ext)
		if err != nil {
			b.log.Warn("could not plan Telegram media path", "error", err.Error())
			b.sendBest(turnCtx, p.Scope, mediaFailure(err))
			return
		}
		mediaPath = path
		prompt = mediaPrompt(prompt, p.Media, path)
	}

	model, err := b.effectiveModel(turnCtx, p.Scope)
	if err != nil {
		b.log.Error("could not resolve model setting", "error", err.Error())
		b.sendBest(turnCtx, p.Scope, "I could not read the model setting, so I did not run Codex.")
		return
	}
	prompt += fileHandoffInstruction
	req := codexcli.Request{ThreadID: sess.CodexThreadID, Prompt: prompt, Model: model, Images: imagePaths}

	// Recording the turn before starting Codex is what makes the row an honest
	// audit trail: if the process dies here, startup finds a 'running' row and
	// reports it as interrupted instead of the turn vanishing.
	turnID, err := b.st.BeginTurn(turnCtx, p.UpdateID, sess.ID, p.Scope.UserID,
		len([]rune(p.Text)), b.cx.ElidedArgv(req))
	if err != nil {
		if errors.Is(err, store.ErrAlreadyClaimed) {
			// The unique index on turns.update_id caught a duplicate that the
			// poller's claim should already have caught. Belt and braces: no
			// second Codex process is started for one Telegram message.
			b.log.Warn("refusing to start a second turn for one update",
				"update_id", p.UpdateID, "session_id", sess.ID)
			return
		}
		b.log.Error("could not record a turn", "update_id", p.UpdateID, "error", err.Error())
		b.sendBest(turnCtx, p.Scope, "I could not record this turn in my database, so I did not run Codex. "+
			"Check the bot log.")
		return
	}
	entry.setTurn(turnID)

	// --- workspace lock -----------------------------------------------------
	if b.wsLocks.Held(workspaceKey(sess.Workspace)) {
		b.sendBest(turnCtx, p.Scope, "⏳ Queued for the workspace. Another turn is using it; /stop cancels this chat's active turn.")
	}
	releaseWS, err := b.wsLocks.Acquire(waitCtx, workspaceKey(sess.Workspace))
	if err != nil {
		// Detached, so the busy turn is still recorded when the wait ended
		// because the user cancelled it.
		busyCtx, cancelBusy := context.WithTimeout(context.WithoutCancel(ctx), afterTurnTimeout)
		defer cancelBusy()
		b.finish(busyCtx, p, sess, turnID, req, turnOutcome{
			status: store.TurnBusy,
			errMsg: "the workspace is in use",
		})
		return
	}
	defer releaseWS()
	currentSession, err := b.st.GetSession(turnCtx, sess.ID, p.Scope.UserID)
	if err != nil {
		b.log.Error("could not check session after acquiring workspace", "session_id", sess.ID, "error", err.Error())
		b.failBeforeRun(ctx, p, sess, turnID, req, "session check failed")
		return
	}
	retained, err := b.st.RetainedMedia(turnCtx, sess.ID, p.Scope.UserID)
	if err != nil {
		b.log.Error("could not load retained media", "session_id", sess.ID, "error", err.Error())
		b.failBeforeRun(ctx, p, sess, turnID, req, "attachment lookup failed")
		return
	}
	if !currentSession.Archived {
		for _, media := range retained {
			if media.UnrelatedTurns >= 3 {
				if err := removeMediaDirectory(sess.Workspace, media.Path); err != nil {
					b.log.Warn("could not remove expired media", "media_id", media.ID, "error", err.Error())
					continue
				}
				if err := b.st.DeleteRetainedMedia(turnCtx, media.ID, p.Scope.UserID); err != nil {
					b.log.Warn("could not forget expired media", "media_id", media.ID, "error", err.Error())
				}
				continue
			}
			if _, err := os.Stat(media.Path); err != nil {
				b.log.Warn("retained media is missing", "media_id", media.ID, "error", err.Error())
				if err := removeMediaDirectory(sess.Workspace, media.Path); err != nil {
					b.log.Warn("could not remove missing media directory", "media_id", media.ID, "error", err.Error())
				}
				if err := b.st.DeleteRetainedMedia(turnCtx, media.ID, p.Scope.UserID); err != nil {
					b.log.Warn("could not forget missing media", "media_id", media.ID, "error", err.Error())
				}
				continue
			}
			available = append(available, media)
		}
	}
	prompt = p.Text
	if p.Media != nil {
		prompt = mediaPrompt(prompt, p.Media, mediaPath)
		if p.Media.image {
			imagePaths = append(imagePaths, mediaPath)
		}
	}
	prompt += retainedMediaPrompt(available)
	req.Prompt = prompt + fileHandoffInstruction
	req.Images = imagePaths
	if err := b.st.UpdateTurnArgv(turnCtx, turnID, p.Scope.UserID, b.cx.ElidedArgv(req)); err != nil {
		b.log.Error("could not record final invocation", "turn_id", turnID, "error", err.Error())
		b.failBeforeRun(ctx, p, sess, turnID, req, "could not record invocation")
		return
	}
	if p.Media != nil {
		cleanup, err := b.stageMedia(turnCtx, p.Media, mediaPath)
		if err == nil && currentSession.Archived {
			defer cleanup()
		} else if err == nil {
			err = b.st.AddRetainedMedia(turnCtx, sess.ID, p.Scope.UserID, mediaPath, p.Media.kind)
			if err != nil {
				cleanup()
			}
		}
		if err != nil {
			b.log.Warn("could not prepare Telegram media", "kind", p.Media.kind, "error", err.Error())
			status := store.TurnFailed
			if errors.Is(err, context.Canceled) {
				status = store.TurnCancelled
			}
			afterCtx, cancelAfter := context.WithTimeout(context.WithoutCancel(ctx), afterTurnTimeout)
			defer cancelAfter()
			if ferr := b.st.FinishTurn(afterCtx, turnID, status, "", "", "attachment preparation failed", 0); ferr != nil {
				b.log.Error("could not record attachment failure", "turn_id", turnID, "error", ferr.Error())
			}
			b.sendBest(afterCtx, p.Scope, mediaFailure(err))
			return
		}
	}

	// From here the wait is over and only Codex can make this slow, so this is
	// where a "still working" acknowledgement starts to be worth sending.
	var finished atomic.Bool
	var ackTimer *time.Timer
	if ack := b.ackMessage(sess, req.IsResume()); ack != "" {
		ackTimer = time.AfterFunc(b.cfg.AckAfter, func() {
			if !finished.Load() {
				b.sendBest(turnCtx, p.Scope, ack)
			}
		})
		defer ackTimer.Stop()
	}

	b.log.Info("running a codex turn",
		"turn_id", turnID, "session_id", sess.ID,
		"update_id", p.UpdateID, "message_id", p.MessageID,
		"resume", req.IsResume(), "thread_id", req.ThreadID,
		"prompt_chars", len([]rune(p.Text)), "sandbox", b.cx.Sandbox(), "model", req.Model)

	startedAt := time.Now().Add(-2 * time.Second)
	entry.setRunning()
	res, runErr := b.cx.Run(turnCtx, req)
	finished.Store(true)
	if ackTimer != nil {
		ackTimer.Stop()
	}

	// Everything from here on must survive the cancellation that may have just
	// happened: if the user sent /stop, turnCtx is already dead, and using it
	// would leave the turn row stuck at 'running' and the thread id unsaved. So
	// bookkeeping and delivery run on a context detached from the turn's.
	afterCtx, cancelAfter := context.WithTimeout(context.WithoutCancel(ctx), afterTurnTimeout)
	defer cancelAfter()

	b.persistThreadID(afterCtx, sess, res)
	if terr := b.st.TouchLastTurn(afterCtx, sess.ID, p.Scope.UserID, b.now()); terr != nil {
		b.log.Error("could not record the last turn time", "session_id", sess.ID, "error", terr.Error())
	}

	out := b.classify(runErr, res)
	out.startedAt = startedAt
	if runErr != nil {
		b.log.Warn("a codex turn did not succeed",
			"turn_id", turnID, "session_id", sess.ID, "status", out.status,
			"exit_code", out.exitCode, "error", redact.OneLine(runErr.Error()))
	} else {
		b.log.Info("a codex turn succeeded",
			"turn_id", turnID, "session_id", sess.ID, "thread_id", out.threadID,
			"duration", durationOf(res), "reply_chars", len([]rune(out.reply)))
	}

	b.finish(afterCtx, p, sess, turnID, req, out)
	if out.status == store.TurnCompleted {
		b.advanceRetainedMedia(afterCtx, sess.Workspace, p.Scope.UserID, available, relatedMediaIDs(out.reply))
	}
}

func (b *Bot) failBeforeRun(ctx context.Context, p *Prepared, sess store.Session, turnID int64, req codexcli.Request, reason string) {
	afterCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), afterTurnTimeout)
	defer cancel()
	b.finish(afterCtx, p, sess, turnID, req, turnOutcome{status: store.TurnFailed, errMsg: reason})
}

// afterTurnTimeout bounds the post-turn bookkeeping and delivery. It is long
// enough for a multi-part Telegram reply on a slow connection and short enough
// that a hung API cannot pin the goroutine.
const afterTurnTimeout = 90 * time.Second

// classify maps a Run result onto a stored status, a user-visible reply and an
// error string for the database.
func (b *Bot) classify(runErr error, res *codexcli.Result) turnOutcome {
	out := turnOutcome{
		res:      res,
		threadID: threadIDOf(res),
		exitCode: exitCodeOf(res),
	}
	if runErr == nil {
		out.status = store.TurnCompleted
		out.reply = replyOf(res)
		return out
	}
	switch {
	case errors.Is(runErr, codexcli.ErrCancelled):
		out.status = store.TurnCancelled
		out.errMsg = "cancelled by the user"
	case errors.Is(runErr, codexcli.ErrTimeout):
		out.status = store.TurnTimeout
		out.errMsg = redact.OneLine(runErr.Error())
	default:
		out.status = store.TurnFailed
		out.errMsg = redact.OneLine(runErr.Error())
	}
	return out
}

// finish records a turn's outcome and tells the user about it.
//
// The caller passes a context that is already detached from the turn's own
// cancellation, so a /stop still produces its "cancelled" message and still
// records the row.
func (b *Bot) finish(ctx context.Context, p *Prepared, sess store.Session, turnID int64,
	req codexcli.Request, out turnOutcome) {

	if err := b.st.FinishTurn(ctx, turnID, out.status, out.threadID, out.reply, out.errMsg, out.exitCode); err != nil {
		b.log.Error("could not record a turn outcome", "turn_id", turnID, "error", err.Error())
	}

	text := b.textFor(out, sess, req)
	if text == "" {
		return
	}
	var err error
	if out.status == store.TurnCompleted {
		err = b.deliverReply(ctx, p.Scope, text, out.startedAt)
	} else {
		err = b.send(ctx, p.Scope, text)
	}
	if err != nil {
		b.log.Error("could not deliver a turn result",
			"turn_id", turnID, "status", out.status, "error", err.Error())
		if out.status == store.TurnCompleted {
			b.sendBest(ctx, p.Scope, "Telegram did not confirm delivery of the generated file or full reply. I will retry automatically; check the bot log if it continues to fail.")
		}
		return
	}
	// Only now is the reply known to have reached Telegram. This flag is what
	// lets a redelivered update stay silent instead of duplicating the answer.
	if out.status == store.TurnCompleted {
		if err := b.st.MarkTurnDelivered(ctx, turnID); err != nil {
			b.log.Error("could not mark a turn delivered", "turn_id", turnID, "error", err.Error())
		}
	}
}

// textFor chooses what the user sees for an outcome.
func (b *Bot) textFor(out turnOutcome, sess store.Session, req codexcli.Request) string {
	switch out.status {
	case store.TurnCompleted:
		return out.reply
	case store.TurnBusy:
		return busyText(sess, out.errMsg)
	case store.TurnCancelled:
		return cancelledText(sess, out.threadID)
	case store.TurnTimeout:
		return timeoutText(b.cfg.TurnTimeout, sess, out.threadID)
	default:
		return b.failureText(out, sess, req)
	}
}

// reportQueueFailure answers a message that could not even get in line.
func (b *Bot) reportQueueFailure(ctx context.Context, p *Prepared, what string, err error) {
	b.log.Warn("a turn could not acquire a lock",
		"user_id", p.Scope.UserID, "lock", what, "error", err.Error())

	sendCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	msg := fmt.Sprintf("I could not start this turn: %s is busy and my wait limit (%s) expired. "+
		"Nothing was sent to Codex. Send /stop to cancel what is running, or try again shortly.",
		what, b.cfg.QueueTimeout)
	if errors.Is(err, context.Canceled) {
		msg = "This turn was cancelled before it started."
	}
	b.sendBest(sendCtx, p.Scope, msg)
}

// ensureSession resolves the scope's selected session, creating one if there is
// none. It returns the session and an optional notice to send first.
func (b *Bot) ensureSession(ctx context.Context, p *Prepared) (store.Session, string, error) {
	sess, err := b.st.SelectedSession(ctx, p.Scope.ChatID, p.Scope.ThreadID, p.Scope.UserID)
	if err == nil {
		return sess, "", nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return store.Session{}, "", err
	}

	// No selection yet. Rather than refusing the message, start a session for
	// it — that is the documented behaviour of an ordinary message — and say so,
	// because an invisible session is a confusing one.
	created, cerr := b.st.CreateSession(ctx, p.Scope.UserID, b.autoName(), b.cfg.Workspace)
	if cerr != nil {
		return store.Session{}, "", cerr
	}
	if serr := b.st.SelectSession(ctx, p.Scope.ChatID, p.Scope.ThreadID, p.Scope.UserID, created.ID); serr != nil {
		return store.Session{}, "", serr
	}
	b.log.Info("created a session automatically for a first message",
		"session_id", created.ID, "user_id", p.Scope.UserID)
	notice := fmt.Sprintf("No session was selected, so I started %s for you. "+
		"Use /new <name> to create named sessions and /use <id> to switch.", created.ID)
	return created, notice, nil
}

// persistThreadID stores the thread id a turn reported.
//
// It runs even for a failed turn: thread.started arrives before the model is
// called, so a turn that failed later still created a thread, and keeping the id
// means the next message resumes that thread instead of silently starting a new
// conversation.
func (b *Bot) persistThreadID(ctx context.Context, sess store.Session, res *codexcli.Result) {
	id := threadIDOf(res)
	if id == "" || id == sess.CodexThreadID {
		return
	}
	if err := b.st.SetThreadID(ctx, sess.ID, sess.OwnerUserID, id); err != nil {
		b.log.Error("could not store a codex thread id",
			"session_id", sess.ID, "thread_id", id, "error", err.Error())
		return
	}
	b.log.Info("recorded a codex thread id",
		"session_id", sess.ID, "thread_id", id, "replaced", sess.CodexThreadID)
}

// --- user-visible text -----------------------------------------------------

func (b *Bot) ackMessage(sess store.Session, resume bool) string {
	if b.cfg.AckAfter <= 0 {
		return ""
	}
	kind := "a new Codex thread"
	if resume {
		kind = "resuming the Codex thread"
	}
	return fmt.Sprintf("⏳ Working on it in session %s (%s). Send /stop to cancel.", sess.ID, kind)
}

func busyText(sess store.Session, reason string) string {
	return fmt.Sprintf("Session %s is in use: %s. Only one Codex turn can touch the workspace at a "+
		"time, because Codex edits files. Send /stop to cancel the running turn, then send this again.",
		sess.ID, orGeneric(reason, "another turn holds the workspace lock"))
}

func cancelledText(sess store.Session, threadID string) string {
	var sb strings.Builder
	sb.WriteString("⏹ Cancelled. The Codex process group was terminated.")
	if threadID != "" {
		fmt.Fprintf(&sb, "\nSession %s still points at thread %s, so you can continue — but Codex "+
			"may already have changed files. Check `git status` in the workspace.", sess.ID, threadID)
	} else {
		fmt.Fprintf(&sb, "\nNo Codex thread was created, so session %s is still empty.", sess.ID)
	}
	return sb.String()
}

func timeoutText(limit time.Duration, sess store.Session, threadID string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "⏱ Codex did not finish within %s and was terminated.", limit)
	if threadID != "" {
		fmt.Fprintf(&sb, "\nSession %s kept thread %s, so the next message resumes it. Codex may have "+
			"left partial edits behind; check `git status`.", sess.ID, threadID)
	} else {
		sb.WriteString("\nNo thread id was reported, so nothing was saved for this turn.")
	}
	sb.WriteString("\nRaise BOT_TURN_TIMEOUT if this task legitimately needs longer.")
	return sb.String()
}

// failureText builds the message for a failed turn.
//
// What it never contains: the Codex argv, the environment, the bot token, hidden
// reasoning, or raw JSONL. What it does contain is Codex's own short reason and a
// bounded, secret-scrubbed stderr tail, because "it failed" on its own gives the
// user nothing to act on.
//
// Classification looks at both streams. That matters: `codex exec resume`
// reports a missing thread on stderr and exits 1 without emitting any JSONL
// event at all, so an event-only classifier would show a generic message for the
// one failure a user can actually do something about.
func (b *Bot) failureText(out turnOutcome, sess store.Session, req codexcli.Request) string {
	haystack := strings.ToLower(failureDetail(out))

	var sb strings.Builder
	switch {
	case strings.Contains(haystack, "no rollout found for thread id"):
		// Codex no longer has the recorded conversation: CODEX_HOME was
		// replaced, the thread was archived or deleted, or the history was
		// pruned. Resuming it can never work again, so say what will.
		fmt.Fprintf(&sb, "Codex no longer has the conversation this session points at (thread %s). "+
			"That history lives in CODEX_HOME, so a restore or a prune there can cause this. "+
			"Send /new to start a fresh session.", orGeneric(req.ThreadID, "unknown"))
	case strings.Contains(haystack, "not inside a trusted directory"),
		strings.Contains(haystack, "skip-git-repo-check"):
		fmt.Fprintf(&sb, "Codex does not accept %s as a Git repository any more. "+
			"Fix the workspace, or restart the bot with a valid BOT_WORKSPACE.", sess.Workspace)
	case strings.Contains(haystack, "not logged in"),
		strings.Contains(haystack, "unauthorized"),
		strings.Contains(haystack, "missing bearer"),
		strings.Contains(haystack, "invalid api key"),
		strings.Contains(haystack, "could not authenticate"):
		sb.WriteString("Codex could not authenticate in the bot's CODEX_HOME. Sign in with " +
			"`CODEX_HOME=<bot codex home> codex login --device-auth`, then send the message again.")
	default:
		sb.WriteString("The Codex turn failed.")
	}

	if reason := failureReason(out); reason != "" {
		fmt.Fprintf(&sb, "\nReason: %s", truncateRunes(reason, 300))
	}
	if tail := stderrTail(out.res, b.cfg.StderrInReply); strings.Count(tail, "\n") > 0 {
		// Only worth showing when it adds lines beyond the one already quoted.
		fmt.Fprintf(&sb, "\nCodex output (tail):\n%s", tail)
	}
	fmt.Fprintf(&sb, "\nSession %s was not changed by this failure and can be retried.", sess.ID)
	if out.res != nil && strings.TrimSpace(out.res.Reply) != "" {
		sb.WriteString("\nCodex produced a partial answer, which I am not forwarding " +
			"because the turn did not complete.")
	}
	return sb.String()
}

// failureDetail is everything Codex said about the failure, on both streams,
// scrubbed and flattened to one line. It is used for classification, so it must
// be exhaustive rather than pretty.
func failureDetail(out turnOutcome) string {
	parts := make([]string, 0, 2)
	if out.errMsg != "" {
		parts = append(parts, out.errMsg)
	}
	if out.res != nil && out.res.Stderr != "" {
		parts = append(parts, out.res.Stderr)
	}
	return redact.OneLine(redact.Secrets(strings.Join(parts, " | ")))
}

// failureReason is the short, user-facing explanation: the runner's own error,
// or the event-stream reason when the runner had nothing to say.
func failureReason(out turnOutcome) string {
	if out.errMsg != "" {
		return redact.OneLine(redact.Secrets(out.errMsg))
	}
	if out.res != nil {
		return redact.OneLine(redact.Secrets(out.res.Events.Reason()))
	}
	return ""
}

// stderrTail returns a bounded, scrubbed excerpt of the child's stderr.
func stderrTail(res *codexcli.Result, max int) string {
	if res == nil || max <= 0 {
		return ""
	}
	s := strings.TrimSpace(res.Stderr)
	if s == "" {
		return ""
	}
	// Codex's own progress lines are noise next to the actual failure, and the
	// reason for a failure is at the end.
	lines := make([]string, 0, 8)
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.Contains(t, "Reading additional input from stdin") {
			continue
		}
		lines = append(lines, t)
	}
	if len(lines) == 0 {
		return ""
	}
	if len(lines) > 8 {
		lines = lines[len(lines)-8:]
	}
	// redact.Tail bounds the result and scrubs credentials in one step.
	return redact.Tail(strings.Join(lines, "\n"), max)
}

func truncateRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

func orGeneric(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

func replyOf(res *codexcli.Result) string {
	if res == nil {
		return ""
	}
	return res.Reply
}

func threadIDOf(res *codexcli.Result) string {
	if res == nil {
		return ""
	}
	return res.ThreadID
}

func exitCodeOf(res *codexcli.Result) int {
	if res == nil {
		return 0
	}
	return res.ExitCode
}

func durationOf(res *codexcli.Result) time.Duration {
	if res == nil {
		return 0
	}
	return res.Duration
}

func scopeKey(s Scope) string {
	if s.ThreadID == 0 {
		return "scope:" + itoa(s.ChatID)
	}
	return "scope:" + itoa(s.ChatID) + ":" + itoa(s.ThreadID)
}

func workspaceKey(path string) string { return "ws:" + path }

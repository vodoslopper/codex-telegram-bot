package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"codex-telegram-bot/internal/sessid"
	"codex-telegram-bot/internal/store"
)

// handleCommand dispatches a parsed command.
func (b *Bot) handleCommand(ctx context.Context, p *Prepared) {
	cmd := p.Cmd
	if cmd.OtherBot {
		// Addressed to a different bot. Answering would be noise, and in a
		// private chat it cannot happen unless the user typed the mention by
		// hand.
		b.log.Debug("ignoring a command addressed to another bot",
			"command", cmd.Name, "user_id", p.Scope.UserID)
		return
	}
	if cmd.Name == "" {
		b.sendBest(ctx, p.Scope, "That does not look like a command. Send /help for the list.")
		return
	}

	b.log.Info("command", "command", cmd.Name,
		"user_id", p.Scope.UserID, "chat_id", p.Scope.ChatID,
		"thread_id", p.Scope.ThreadID, "args", len(cmd.Args))

	var err error
	switch cmd.Name {
	case "start":
		b.sendBest(ctx, p.Scope, b.helpText(ctx, p.Scope, true))
	case "help", "commands":
		b.sendBest(ctx, p.Scope, b.helpText(ctx, p.Scope, false))
	case "new":
		err = b.cmdNew(ctx, p, cmd)
	case "sessions", "ls":
		err = b.cmdSessions(ctx, p, cmd)
	case "use", "switch":
		err = b.cmdUse(ctx, p, cmd)
	case "session", "status", "current":
		err = b.cmdSession(ctx, p)
	case "model":
		err = b.cmdModel(ctx, p, cmd)
	case "rename":
		err = b.cmdRename(ctx, p, cmd)
	case "archive":
		err = b.cmdArchive(ctx, p, cmd, true)
	case "unarchive":
		err = b.cmdArchive(ctx, p, cmd, false)
	case "stop", "cancel":
		b.cmdStop(ctx, p)
		return
	default:
		b.sendBest(ctx, p.Scope, fmt.Sprintf("Unknown command /%s. Send /help for the list.", cmd.Name))
		return
	}
	if err != nil {
		b.log.Error("command failed", "command", cmd.Name,
			"user_id", p.Scope.UserID, "error", err.Error())
		b.sendBest(ctx, p.Scope, commandErrorText(cmd.Name, err))
	}
}

// commandErrorText turns a store error into something a person can act on,
// without exposing SQL or internal state.
func commandErrorText(cmd string, err error) string {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return fmt.Sprintf("/%s: I could not find that session. It may belong to another user, "+
			"or the id may be mistyped. /sessions lists yours.", cmd)
	case errors.Is(err, context.Canceled):
		return fmt.Sprintf("/%s: cancelled.", cmd)
	default:
		return fmt.Sprintf("/%s: something went wrong on my side (%s). Check the bot log for details.",
			cmd, firstLine(err.Error()))
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// --- /start and /help ------------------------------------------------------

func (b *Bot) helpText(ctx context.Context, scope Scope, greeting bool) string {
	var sb strings.Builder
	if greeting {
		sb.WriteString("Codex bot. I run one Codex CLI session per chat, in a Git workspace you control.\n\n")
	}
	sb.WriteString(`Commands:
/new [name]         create a session and select it
/sessions [all]     list your sessions (all includes archived)
/use <id>           switch to one of your sessions
/session            show the selected session, workspace and status
/model              show the model used in this chat or topic
/model luna|sol     select a model here; /model reset inherits your default
/model default luna|sol  set your default for other chats and topics
/rename <id> <name> rename a session
/archive <id>       hide a session from /sessions (Codex history is kept)
/unarchive <id>     show an archived session again
/stop               cancel the turn running in this chat
/help               this message

`)
	sb.WriteString("Anything else you send becomes a prompt for Codex. " +
		"If no session is selected yet, one is created for you.\n\n")

	if sel, err := b.st.SelectedSession(ctx, scope.ChatID, scope.ThreadID, scope.UserID); err == nil {
		fmt.Fprintf(&sb, "Selected: %s\n", sessionHeading(sel))
	} else {
		sb.WriteString("Selected: none — your next message starts a new session.\n")
	}
	fmt.Fprintf(&sb, "Workspace: %s\n", b.cfg.Workspace)
	fmt.Fprintf(&sb, "Sandbox: %s\n", b.cx.Sandbox())
	if model, err := b.effectiveModel(ctx, scope); err == nil {
		fmt.Fprintf(&sb, "Model: %s\n", model)
	}
	if b.codexVersion != "" {
		fmt.Fprintf(&sb, "Codex: %s\n", b.codexVersion)
	}
	if scope.ThreadID != 0 {
		fmt.Fprintf(&sb, "Topic: %d (each topic keeps its own selection)\n", scope.ThreadID)
	}
	return sb.String()
}

// --- /new ------------------------------------------------------------------

func (b *Bot) cmdNew(ctx context.Context, p *Prepared, cmd *Command) error {
	name := strings.TrimSpace(cmd.Rest)
	if name == "" {
		name = b.autoName()
	}
	if len(name) > 120 {
		return errors.New("that name is longer than 120 characters")
	}
	sess, err := b.st.CreateSession(ctx, p.Scope.UserID, name, b.cfg.Workspace)
	if err != nil {
		return err
	}
	if err := b.st.SelectSession(ctx, p.Scope.ChatID, p.Scope.ThreadID, p.Scope.UserID, sess.ID); err != nil {
		return err
	}
	b.sendBest(ctx, p.Scope, fmt.Sprintf(
		"Created session %s.\nIt is selected here; your next message goes to Codex.", sess.ID))
	return nil
}

// autoName labels a session the bot had to create by itself. It is a timestamp
// rather than an excerpt of the message: the name is stored in the database and
// shown in listings, and copying user text into it would put message content
// somewhere the operator did not ask for.
func (b *Bot) autoName() string {
	return "auto " + b.now().Format("2006-01-02 15:04 UTC")
}

// --- /sessions -------------------------------------------------------------

func (b *Bot) cmdSessions(ctx context.Context, p *Prepared, cmd *Command) error {
	includeArchived := len(cmd.Args) > 0 && strings.EqualFold(cmd.Args[0], "all")
	sessions, err := b.st.ListSessions(ctx, p.Scope.UserID, includeArchived)
	if err != nil {
		return err
	}
	if len(sessions) == 0 {
		msg := "You have no sessions yet. Send /new [name] to create one, or just send a message."
		if includeArchived {
			msg = "You have no sessions, archived or otherwise. Send /new [name] to create one."
		}
		b.sendBest(ctx, p.Scope, msg)
		return nil
	}

	selected, selErr := b.selectedID(ctx, p.Scope)
	var sb strings.Builder
	fmt.Fprintf(&sb, "Your sessions (%d):\n", len(sessions))
	for _, s := range sessions {
		marker := " "
		if s.ID == selected {
			marker = "*"
		}
		fmt.Fprintf(&sb, "%s %s  %s\n", marker, s.ID, sessionSummaryLine(s))
	}
	sb.WriteString("\n* = selected here. /use <id> to switch, /archive <id> to hide.")
	if !includeArchived {
		sb.WriteString(" /sessions all to include archived.")
	}
	b.sendBest(ctx, p.Scope, sb.String())
	_ = selErr
	return nil
}

func sessionSummaryLine(s store.Session) string {
	parts := make([]string, 0, 3)
	parts = append(parts, orPlaceholder(s.Name))
	if s.Archived {
		parts = append(parts, "archived")
	}
	if !s.HasThread() {
		parts = append(parts, "no thread yet")
	} else if !s.LastTurnAt.IsZero() {
		parts = append(parts, "last turn "+s.LastTurnAt.Format("2006-01-02 15:04 UTC"))
	}
	return strings.Join(parts, " · ")
}

func orPlaceholder(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(unnamed)"
	}
	return s
}

// --- /use ------------------------------------------------------------------

func (b *Bot) cmdUse(ctx context.Context, p *Prepared, cmd *Command) error {
	id, err := sessionArg(cmd, "use")
	if err != nil {
		b.sendBest(ctx, p.Scope, err.Error())
		return nil
	}
	// GetSession checks ownership; SelectSession checks it again inside the
	// statement that writes. The first check is what produces a good message.
	sess, err := b.st.GetSession(ctx, id, p.Scope.UserID)
	if err != nil {
		return err
	}
	if err := b.st.SelectSession(ctx, p.Scope.ChatID, p.Scope.ThreadID, p.Scope.UserID, id); err != nil {
		return err
	}
	b.sendBest(ctx, p.Scope, fmt.Sprintf("Selected %s.\n%s", id, sessionDetail(ctx, b, p.Scope, sess)))
	return nil
}

// --- /session --------------------------------------------------------------

func (b *Bot) cmdSession(ctx context.Context, p *Prepared) error {
	sess, err := b.st.SelectedSession(ctx, p.Scope.ChatID, p.Scope.ThreadID, p.Scope.UserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			b.sendBest(ctx, p.Scope, "No session is selected here. Send /new [name] to create one, "+
				"or just send a message and I will start one.")
			return nil
		}
		return err
	}
	b.sendBest(ctx, p.Scope, sessionDetail(ctx, b, p.Scope, sess))
	return nil
}

// sessionDetail renders the /session answer, including live turn state.
func sessionDetail(ctx context.Context, b *Bot, scope Scope, sess store.Session) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Session %s — %s\n", sess.ID, orPlaceholder(sess.Name))
	fmt.Fprintf(&sb, "Workspace: %s\n", sess.Workspace)
	if sess.HasThread() {
		fmt.Fprintf(&sb, "Codex thread: %s\n", sess.CodexThreadID)
	} else {
		sb.WriteString("Codex thread: none yet (the first message creates one)\n")
	}
	fmt.Fprintf(&sb, "Status: %s\n", b.statusText(ctx, scope, sess))
	if sess.Archived {
		sb.WriteString("Archived: yes (hidden from /sessions; /unarchive to restore)\n")
	}
	fmt.Fprintf(&sb, "Created: %s\n", sess.CreatedAt.Format(time.RFC3339))
	return strings.TrimRight(sb.String(), "\n")
}

// statusText reports what is happening with a session right now.
//
// Three sources, in order of authority: the in-memory registry for this scope (a
// turn this process is running for this very chat), the database (a turn running
// for the session from another scope, or one left over from a crash), and the
// last recorded turn.
func (b *Bot) statusText(ctx context.Context, scope Scope, sess store.Session) string {
	if t := b.inflight.get(scope); t != nil && t.sessionID == sess.ID {
		return fmt.Sprintf("running for %s here (started %s ago) — /stop to cancel",
			t.sessionID, b.now().Sub(t.started).Round(time.Second))
	}
	if t := b.inflight.get(scope); t != nil {
		return fmt.Sprintf("idle, but %s is running in this chat — /stop to cancel", t.sessionID)
	}
	if running, err := b.st.HasRunningTurn(ctx, sess.ID); err == nil && running {
		return "running from another chat or topic of yours"
	}
	last, err := b.st.LastTurnForSession(ctx, sess.ID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "idle (no turn has run yet)"
		}
		return "idle (last turn unknown)"
	}
	when := last.FinishedAt
	if when.IsZero() {
		when = last.StartedAt
	}
	line := fmt.Sprintf("idle — last turn %s at %s", last.Status, when.Format("2006-01-02 15:04:05 UTC"))
	if last.Status == store.TurnInterrupted {
		line += "\nThat turn was cut short by a bot restart. Codex may have made partial changes; " +
			"check `git status` in the workspace before continuing."
	}
	return line
}

// --- /rename, /archive -----------------------------------------------------

func (b *Bot) cmdRename(ctx context.Context, p *Prepared, cmd *Command) error {
	id, remainder := firstField(cmd.Rest)
	if id == "" {
		b.sendBest(ctx, p.Scope, "Usage: /rename <session-id> <new name>")
		return nil
	}
	if !sessid.Valid(id) {
		b.sendBest(ctx, p.Scope, badIDText(id))
		return nil
	}
	name := strings.TrimSpace(remainder)
	if name == "" {
		b.sendBest(ctx, p.Scope, "Usage: /rename <session-id> <new name> — the name may contain spaces.")
		return nil
	}
	if len(name) > 120 {
		b.sendBest(ctx, p.Scope, "That name is longer than 120 characters.")
		return nil
	}
	if err := b.st.RenameSession(ctx, id, p.Scope.UserID, name); err != nil {
		return err
	}
	b.sendBest(ctx, p.Scope, fmt.Sprintf("Renamed %s to %q.", id, name))
	return nil
}

func (b *Bot) cmdArchive(ctx context.Context, p *Prepared, cmd *Command, archive bool) error {
	verb := "archive"
	if !archive {
		verb = "unarchive"
	}
	id, err := sessionArg(cmd, verb)
	if err != nil {
		b.sendBest(ctx, p.Scope, err.Error())
		return nil
	}
	sess, err := b.st.GetSession(ctx, id, p.Scope.UserID)
	if err != nil {
		return err
	}
	if archive {
		waitCtx, cancel := context.WithTimeout(ctx, b.cfg.QueueTimeout)
		defer cancel()
		release, err := b.wsLocks.Acquire(waitCtx, workspaceKey(sess.Workspace))
		if err != nil {
			return fmt.Errorf("workspace is busy; retry /archive after the active turn finishes: %w", err)
		}
		defer release()
	}
	if err := b.st.SetArchived(ctx, id, p.Scope.UserID, archive); err != nil {
		return err
	}
	if archive {
		media, err := b.st.RetainedMedia(ctx, id, p.Scope.UserID)
		if err != nil {
			return err
		}
		var cleanupErrors []error
		for _, m := range media {
			if err := removeMediaDirectory(sess.Workspace, m.Path); err != nil {
				cleanupErrors = append(cleanupErrors, err)
				continue
			}
			if err := b.st.DeleteRetainedMedia(ctx, m.ID, p.Scope.UserID); err != nil {
				cleanupErrors = append(cleanupErrors, err)
			}
		}
		if len(cleanupErrors) != 0 {
			return fmt.Errorf("session archived, but some attachments could not be removed: %w", errors.Join(cleanupErrors...))
		}
		b.sendBest(ctx, p.Scope, fmt.Sprintf(
			"Archived %s. It is hidden from /sessions but its Codex history is untouched; "+
				"/sessions all still lists it and /unarchive %s brings it back.", id, id))
	} else {
		b.sendBest(ctx, p.Scope, fmt.Sprintf("Unarchived %s.", id))
	}
	return nil
}

// --- /stop -----------------------------------------------------------------

// cmdStop cancels the turn running (or queued) for this scope.
//
// It cancels the turn, not the session: the session keeps its Codex thread id
// and can be resumed with the next message. The cancellation is delivered by
// killing the Codex process group; the reply comes from the turn handler, which
// notices its context end.
func (b *Bot) cmdStop(ctx context.Context, p *Prepared) {
	t := b.inflight.get(p.Scope)
	if t == nil {
		b.sendBest(ctx, p.Scope, "Nothing is running in this chat.")
		return
	}
	b.log.Info("cancelling a turn on request",
		"user_id", p.Scope.UserID, "chat_id", p.Scope.ChatID,
		"thread_id", p.Scope.ThreadID, "session_id", t.sessionID, "turn_id", t.turnID)
	t.cancel()
	b.sendBest(ctx, p.Scope, fmt.Sprintf(
		"Cancelling the turn on %s. The session itself is kept and can be resumed.", t.sessionID))
}

// --- shared argument handling ---------------------------------------------

// sessionArg extracts and validates a session id argument.
func sessionArg(cmd *Command, verb string) (string, error) {
	if len(cmd.Args) < 1 || cmd.Args[0] == "" {
		return "", fmt.Errorf("Usage: /%s <session-id>. /sessions lists yours.", verb)
	}
	id := cmd.Args[0]
	if !sessid.Valid(id) {
		return "", errors.New(badIDText(id))
	}
	return id, nil
}

// badIDText explains a malformed id. It is worth being specific here, because
// the most common mistake is pasting a Codex thread UUID where a bot session id
// belongs — and accepting one would be exactly the ambiguity the two id spaces
// exist to prevent.
func badIDText(id string) string {
	if len(id) > 24 {
		return "That looks like a Codex thread UUID, not a bot session id. " +
			"Use the short id from /sessions, for example s7k3qm."
	}
	return fmt.Sprintf("%q is not a valid session id. Mine look like s7k3qm; /sessions lists yours.", id)
}

func sessionHeading(s store.Session) string {
	name := orPlaceholder(s.Name)
	if s.HasThread() {
		return fmt.Sprintf("%s — %q (thread %s)", s.ID, name, s.CodexThreadID)
	}
	return fmt.Sprintf("%s — %q (no Codex thread yet)", s.ID, name)
}

// selectedID returns the scope's current selection, or "" when there is none.
func (b *Bot) selectedID(ctx context.Context, scope Scope) (string, error) {
	sess, err := b.st.SelectedSession(ctx, scope.ChatID, scope.ThreadID, scope.UserID)
	if err != nil {
		return "", err
	}
	return sess.ID, nil
}

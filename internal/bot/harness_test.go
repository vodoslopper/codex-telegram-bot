package bot

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"codex-telegram-bot/internal/codexcli"
	"codex-telegram-bot/internal/config"
	"codex-telegram-bot/internal/store"
	"codex-telegram-bot/internal/telegram"
	"codex-telegram-bot/internal/testkit"
)

// Identities used across the tests. The allowlist is alice and bob; mallory is
// not on it, and that difference is the whole point of several tests.
const (
	aliceID   = int64(111)
	bobID     = int64(222)
	malloryID = int64(333)

	aliceChat = int64(111)
	bobChat   = int64(222)
)

// harness is a fully wired bot with an in-memory Telegram and a fake Codex.
type harness struct {
	t      *testing.T
	cfg    *config.Config
	st     *store.Store
	tg     *testkit.FakeTelegram
	fake   *testkit.FakeCodex
	runner *codexcli.Runner
	b      *Bot

	ws    string
	state string
	home  string
}

// harnessOpts configures a harness. Empty fields get a fresh temporary value,
// which is what a restart test overrides to reuse the directories of an earlier
// harness.
type harnessOpts struct {
	spec  testkit.CodexSpec
	fake  *testkit.FakeCodex // reuse an existing fake Codex instead of making one
	ws    string
	state string
	home  string
	env   map[string]string // extra or overriding configuration
	// logSink receives the bot's log output, so a test can assert on what was
	// and was not logged. nil discards it.
	logSink io.Writer
}

func newHarness(t *testing.T, opts harnessOpts) *harness {
	t.Helper()
	log := testLog(opts.logSink)

	ws := opts.ws
	if ws == "" {
		ws = testkit.Workspace(t)
	}
	state := opts.state
	if state == "" {
		state = testkit.StateDir(t)
	}
	home := opts.home
	if home == "" {
		home = testkit.CodexHome(t)
	}
	fake := opts.fake
	if fake == nil {
		fake = testkit.NewFakeCodex(t, opts.spec)
	}

	env := map[string]string{
		"TELEGRAM_BOT_TOKEN":        "123456:AAtest-token-value",
		"ALLOWED_TELEGRAM_USER_IDS": "111,222",
		"BOT_WORKSPACE":             ws,
		"BOT_STATE_DIR":             state,
		"BOT_CODEX_BIN":             fake.Path,
		"CODEX_HOME":                home,
		// Short, so tests that exercise the queue or the timeout stay quick.
		"BOT_TURN_TIMEOUT":  "30s",
		"BOT_QUEUE_TIMEOUT": "2s",
		// No acknowledgement message by default: most tests assert on the reply
		// and an extra message would only have to be filtered out again.
		"BOT_ACK_AFTER":         "0s",
		"BOT_LOG_PROMPTS":       "false",
		"BOT_TELEGRAM_API_BASE": "http://127.0.0.1:1",
	}
	for k, v := range opts.env {
		env[k] = v
	}

	cfg, err := config.Load(env)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}

	st, err := store.Open(cfg.DBPath, log)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	runner, err := codexcli.NewRunner(codexcli.Config{
		Bin:            cfg.CodexBin,
		Workspace:      cfg.Workspace,
		CodexHome:      cfg.CodexHome,
		Sandbox:        cfg.SandboxMode,
		Model:          cfg.Model,
		StrictConfig:   cfg.StrictConfig,
		Timeout:        cfg.TurnTimeout,
		KillGrace:      500 * time.Millisecond,
		MaxStderrBytes: cfg.MaxStderrBytes,
		MaxEventBytes:  cfg.MaxEventBytes,
		BaseEnv: []string{
			"PATH=" + os.Getenv("PATH"),
			"HOME=" + home,
		},
	}, log)
	if err != nil {
		st.Close()
		t.Fatalf("codexcli.NewRunner: %v", err)
	}

	tg := testkit.NewFakeTelegram()
	b := New(cfg, tg, st, runner, log)
	b.SetCodexVersion("codex-cli 0.0.0-test")
	b.botUsername = tg.Username()

	h := &harness{t: t, cfg: cfg, st: st, tg: tg, fake: fake, runner: runner, b: b,
		ws: ws, state: state, home: home}
	t.Cleanup(h.Close)
	return h
}

// Close releases the database. A restart test closes the old harness before
// opening a new one on the same files.
func (h *harness) Close() {
	if h.st != nil {
		h.st.Close()
		h.st = nil
	}
}

// say delivers one update synchronously and returns the messages the bot sent in
// response.
func (h *harness) say(u telegram.Update) []testkit.Sent {
	before := len(h.tg.Sent())
	h.b.HandleUpdate(context.Background(), u)
	all := h.tg.Sent()
	if len(all) <= before {
		return nil
	}
	return all[before:]
}

// text delivers one update and returns everything the bot replied, joined.
// Split replies therefore read as one string, which is what most assertions
// want.
func (h *harness) text(u telegram.Update) string {
	msgs := h.say(u)
	parts := make([]string, 0, len(msgs))
	for _, m := range msgs {
		parts = append(parts, m.Text)
	}
	return strings.Join(parts, "\n")
}

// msg builds a private-chat message update from an allowlisted-or-not user.
func msg(updateID, chatID, userID int64, text string) telegram.Update {
	return testkit.MessageUpdate(updateID, chatID, userID, text)
}

// lastTurn returns the most recent turn row for a session.
func (h *harness) lastTurn(t *testing.T, sessionID string) store.Turn {
	t.Helper()
	tu, err := h.st.LastTurnForSession(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("LastTurnForSession(%q): %v", sessionID, err)
	}
	return tu
}

// sessions returns a user's sessions, failing the test on error.
func (h *harness) sessions(t *testing.T, owner int64, all bool) []store.Session {
	t.Helper()
	out, err := h.st.ListSessions(context.Background(), owner, all)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	return out
}

// testLog discards log output unless a sink is given or BOT_TEST_LOG is set, in
// which case it goes to stderr so a failing test can be diagnosed without
// editing code.
func testLog(sink io.Writer) *slog.Logger {
	w := sink
	if w == nil {
		w = io.Discard
		if os.Getenv("BOT_TEST_LOG") != "" {
			w = os.Stderr
		}
	}
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// mustRunnerWithTimeout rebuilds a harness's runner with a shorter deadline than
// BOT_TURN_TIMEOUT allows, so a timeout test does not have to wait half an hour.
func mustRunnerWithTimeout(t *testing.T, h *harness, d time.Duration) *codexcli.Runner {
	t.Helper()
	r, err := codexcli.NewRunner(codexcli.Config{
		Bin:            h.cfg.CodexBin,
		Workspace:      h.cfg.Workspace,
		CodexHome:      h.cfg.CodexHome,
		Sandbox:        h.cfg.SandboxMode,
		Model:          h.cfg.Model,
		StrictConfig:   h.cfg.StrictConfig,
		Timeout:        d,
		KillGrace:      300 * time.Millisecond,
		MaxStderrBytes: h.cfg.MaxStderrBytes,
		MaxEventBytes:  h.cfg.MaxEventBytes,
		BaseEnv:        []string{"PATH=" + os.Getenv("PATH"), "HOME=" + h.home},
	}, testLog(nil))
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return r
}

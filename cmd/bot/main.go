// Command codex-telegram-bot lets an allowlisted Telegram user drive a local
// Codex CLI session from a private chat.
//
// Startup order matters and is the reason this file is mostly straight-line
// code: every check that can fail runs before the first getUpdates, so a
// misconfigured bot refuses to start rather than failing on somebody's first
// message hours later.
//
//  1. Parse and validate the configuration.
//  2. Take the single-poller lock. Two pollers would steal updates from each
//     other by advancing a shared offset.
//  3. Open the database and migrate it.
//  4. Flip any 'running' turn left by a previous crash to 'interrupted'.
//  5. Probe the Codex binary (--version) and its login state.
//  6. Connect to Telegram, clear any webhook, then poll.
//  7. On a signal: stop polling, let in-flight turns finish within the grace
//     period, then cancel, close the database, release the lock.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"codex-telegram-bot/internal/bot"
	"codex-telegram-bot/internal/codexcli"
	"codex-telegram-bot/internal/config"
	"codex-telegram-bot/internal/lockfile"
	"codex-telegram-bot/internal/redact"
	"codex-telegram-bot/internal/store"
	"codex-telegram-bot/internal/telegram"
)

// Version is set at build time with -ldflags, and falls back to "dev".
var Version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "codex-telegram-bot: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	// The logger exists before the configuration does, so a configuration error
	// can still be reported properly. The token is never passed to it.
	log := newLogger("info")

	cfg, err := config.Load(envMap())
	if err != nil {
		return err
	}
	log = newLogger(cfg.LogLevel)
	logStartup(cfg, log)

	// --- single poller ------------------------------------------------------
	lock, err := lockfile.Acquire(cfg.LockPath)
	if err != nil {
		if errors.Is(err, lockfile.ErrHeld) {
			// Note: cfg.LockPath, not lock.Path() — lock is nil here, and
			// dereferencing it would turn a clean refusal into a segfault.
			return fmt.Errorf("another codex-telegram-bot is already polling (lock %s is held); "+
				"stop it first — two pollers would steal updates from each other", cfg.LockPath)
		}
		return err
	}
	defer func() {
		if cerr := lock.Close(); cerr != nil {
			log.Warn("could not release the poller lock", "error", cerr.Error())
		}
	}()
	log.Info("holding the single-poller lock", "path", lock.Path())

	// --- database -----------------------------------------------------------
	st, err := store.Open(cfg.DBPath, log)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := st.Close(); cerr != nil {
			log.Error("could not close the database cleanly", "error", cerr.Error())
		}
	}()

	setupCtx, cancelSetup := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancelSetup()

	// A 'running' turn at startup can only be one a previous process died
	// during. Marking it makes the interruption visible in /session instead of
	// leaving a row that claims a Codex process is alive when none is.
	interrupted, err := st.MarkInterruptedTurns(setupCtx)
	if err != nil {
		return fmt.Errorf("recover turn state: %w", err)
	}
	if interrupted > 0 {
		log.Warn("found turns that were running when the bot last stopped",
			"count", interrupted,
			"note", "Codex may have left partial edits in the workspace; check git status")
	}

	offset, err := st.GetOffset(setupCtx)
	if err != nil {
		return fmt.Errorf("read the update offset: %w", err)
	}
	sessions, err := st.CountAll(setupCtx)
	if err != nil {
		log.Warn("could not count stored sessions", "error", err.Error())
	}
	log.Info("state loaded",
		"db", cfg.DBPath, "telegram_offset", offset, "sessions", sessions,
		"migrations", strings.Join(store.Migrations(), ","))

	// --- codex --------------------------------------------------------------
	runner, err := codexcli.NewRunner(codexcli.Config{
		Bin:            cfg.CodexBin,
		Workspace:      cfg.Workspace,
		CodexHome:      cfg.CodexHome,
		Sandbox:        cfg.SandboxMode,
		Model:          cfg.Model,
		StrictConfig:   cfg.StrictConfig,
		Timeout:        cfg.TurnTimeout,
		KillGrace:      cfg.KillGrace,
		MaxStderrBytes: cfg.MaxStderrBytes,
		MaxEventBytes:  cfg.MaxEventBytes,
	}, log)
	if err != nil {
		return err
	}

	version, err := runner.Version(setupCtx)
	if err != nil {
		return fmt.Errorf("BOT_CODEX_BIN=%s cannot be run: %w", cfg.CodexBin, err)
	}
	log.Info("codex cli found", "bin", cfg.CodexBin, "version", version,
		"sandbox", cfg.SandboxMode, "codex_home", cfg.CodexHome, "workspace", cfg.Workspace)

	if cfg.CheckLogin {
		loggedIn, detail, lerr := runner.LoginStatus(setupCtx)
		switch {
		case lerr != nil:
			// Not fatal: a probe can fail for reasons that have nothing to do
			// with credentials, and the first turn would report it anyway.
			log.Warn("could not check the codex login state", "error", lerr.Error())
		case !loggedIn:
			log.Warn("codex is not logged in under this CODEX_HOME; every turn will fail until it is",
				"codex_home", cfg.CodexHome, "status", detail,
				"fix", fmt.Sprintf("CODEX_HOME=%s %s login --device-auth", cfg.CodexHome, cfg.CodexBin))
		default:
			log.Info("codex login ok", "status", detail)
		}
	}

	// --- telegram -----------------------------------------------------------
	tg, err := telegram.New(cfg.TelegramToken, cfg.TelegramAPIBase, cfg.HTTPTimeout, log,
		telegram.WithMaxRetries(4))
	if err != nil {
		return fmt.Errorf("telegram client: %w", err)
	}
	log.Info("telegram client ready",
		"api_base", cfg.TelegramAPIBase, "token", redact.Token(cfg.TelegramToken),
		"allowed_users", cfg.AllowedUserIDs, "delete_webhook", cfg.DeleteWebhook)

	b := bot.New(cfg, tg, st, runner, log)
	b.SetCodexVersion(version)

	// --- run and shut down --------------------------------------------------
	pollCtx, cancelPoll := context.WithCancel(context.Background())
	workCtx, cancelWork := context.WithCancel(context.Background())
	defer cancelPoll()
	defer cancelWork()

	sig := make(chan os.Signal, 2)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)

	// The first signal stops the poller and starts the grace period, so a Codex
	// turn that is editing files gets a chance to finish. A second signal means
	// "stop now" and cancels the turns outright.
	go func() {
		seen := 0
		for s := range sig {
			seen++
			if seen == 1 {
				log.Info("shutdown signal received; stopping the poller and letting in-flight turns finish",
					"signal", s.String(), "grace", cfg.ShutdownGrace)
				cancelPoll()
				continue
			}
			log.Warn("second shutdown signal; cancelling in-flight turns immediately", "signal", s.String())
			cancelWork()
			cancelPoll()
			return
		}
	}()

	pollErr := b.Run(pollCtx, workCtx)
	if pollErr != nil {
		log.Error("the update poller stopped with an error", "error", pollErr.Error())
		cancelPoll()
	} else {
		log.Info("the update poller stopped")
	}

	handlers := make(chan struct{})
	go func() { b.Wait(); close(handlers) }()
	select {
	case <-handlers:
		log.Info("every in-flight turn finished")
	case <-time.After(cfg.ShutdownGrace):
		log.Warn("the shutdown grace period expired; cancelling in-flight turns",
			"grace", cfg.ShutdownGrace)
		cancelWork()
		<-handlers
	}
	cancelWork()

	if pollErr != nil {
		return fmt.Errorf("poller: %w", pollErr)
	}
	log.Info("shutdown complete")
	return nil
}

// envMap snapshots the process environment for config.Load, which takes a map so
// tests can exercise it without touching os.Setenv.
func envMap() map[string]string {
	env := os.Environ()
	m := make(map[string]string, len(env))
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if ok {
			m[k] = v
		}
	}
	return m
}

// newLogger builds the process logger. Output goes to stderr, which journald
// captures for a systemd --user unit.
//
// The format is text rather than JSON: this is a single-operator service read
// with `journalctl -f`, not a log pipeline. Fields are key=value, so it is still
// machine-parseable.
func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: lvl,
		ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			// A token in a log is a leaked token. Nothing in this program logs
			// one, but dropping any attribute with a secret-sounding key makes
			// that a property of the logger rather than of every call site.
			switch strings.ToLower(a.Key) {
			case "token", "telegram_bot_token", "authorization", "api_key", "apikey", "secret", "password":
				return slog.String(a.Key, "[redacted]")
			}
			return a
		},
	})
	return slog.New(h).With("component", "codex-telegram-bot", "version", Version)
}

// logStartup writes the effective configuration, minus secrets.
//
// Every path is logged because "which CODEX_HOME am I actually using" is the
// first question when a bot behaves as if it had amnesia, and the answer must be
// in the journal rather than in somebody's shell history.
func logStartup(cfg *config.Config, log *slog.Logger) {
	log.Info("starting",
		"workspace", cfg.Workspace,
		"state_dir", cfg.StateDir,
		"db", cfg.DBPath,
		"codex_bin", cfg.CodexBin,
		"codex_home", cfg.CodexHome,
		"sandbox", cfg.SandboxMode,
		"model", orNone(cfg.Model),
		"strict_config", cfg.StrictConfig,
		"turn_timeout", cfg.TurnTimeout,
		"queue_timeout", cfg.QueueTimeout,
		"allowed_users", cfg.AllowedUserIDs,
		"log_prompts", cfg.LogPrompts)

	// A state directory that other local users can read is worth saying out
	// loud: the database holds conversation replies.
	if perm, err := store.FileModeOf(cfg.StateDir); err == nil && perm&0o077 != 0 {
		log.Warn("BOT_STATE_DIR is readable by other users; the database holds conversation replies",
			"path", cfg.StateDir, "mode", perm.String())
	}
	if perm, err := store.FileModeOf(cfg.CodexHome); err == nil && perm&0o077 != 0 {
		log.Warn("CODEX_HOME is readable by other users; it holds the Codex credential cache",
			"path", cfg.CodexHome, "mode", perm.String())
	}
	if rel, err := filepath.Rel(cfg.Workspace, cfg.DBPath); err == nil && !strings.HasPrefix(rel, "..") {
		// Cannot happen: config.Load rejects it. Kept as a last-line warning so
		// the invariant is asserted where it matters rather than only where it
		// is checked.
		log.Error("the database is inside the workspace, which config validation should have rejected",
			"workspace", cfg.Workspace, "db", cfg.DBPath)
	}
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "none"
	}
	return s
}

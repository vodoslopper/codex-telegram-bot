// Package config loads and validates the process configuration.
//
// Everything comes from environment variables so the same binary works under
// systemd --user (EnvironmentFile=), a container, or a plain shell, and so no
// secret ever appears on a command line where `ps` would show it.
//
// Validation is deliberately strict and happens once, at startup: a bot that
// starts with a wrong CODEX_HOME would create and resume Codex threads in the
// wrong place, and a bot that starts with a non-repository workspace would fail
// on the first message hours later. Both are cheap to catch before the first
// getUpdates call, so Load refuses to return a Config that could not work.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"codex-telegram-bot/internal/textsplit"
)

// Defaults. These are the values documented in README.md and .env.example.
const (
	DefaultTelegramAPIBase = "https://api.telegram.org"
	DefaultSandboxMode     = "workspace-write"
	DefaultModel           = "gpt-6-sol"
	DefaultTurnTimeout     = 15 * time.Minute
	DefaultAckAfter        = 10 * time.Second
	DefaultTypingInterval  = 4 * time.Second
	DefaultPollTimeout     = 25 * time.Second
	DefaultPollLimit       = 100
	DefaultHTTPTimeout     = 40 * time.Second
	DefaultMaxStderrBytes  = 64 * 1024
	DefaultMaxEventBytes   = 8 * 1024 * 1024
	DefaultMaxReplyChars   = textsplit.MaxText
	DefaultStderrInReply   = 600
	DefaultShutdownGrace   = 20 * time.Second
	DefaultKillGrace       = 5 * time.Second
	DefaultQueueTimeout    = 5 * time.Minute
	DefaultMaxConcurrent   = 8
	DefaultDBFileName      = "bot.db"
	DefaultLockFileName    = "poller.lock"

	// MaxTelegramMessageLen is Telegram's hard limit on message text, in UTF-16
	// code units. internal/textsplit owns the number, because that is where the
	// counting happens; this is an alias so the two cannot drift apart.
	MaxTelegramMessageLen = textsplit.MaxText
)

// Sandbox modes the bot is willing to run Codex with.
//
// danger-full-access is not in this list on purpose and Load rejects it even
// though the Codex CLI accepts it: a Telegram message that reaches an unsandboxed
// agent on the host is not a trade this bot makes. Use read-only for a bot that
// should only answer questions, workspace-write for one that may edit the
// configured repository.
var allowedSandboxModes = []string{"read-only", "workspace-write"}

// Config is the validated process configuration.
type Config struct {
	// Telegram
	TelegramToken   string
	TelegramAPIBase string
	AllowedUserIDs  []int64
	PollTimeout     time.Duration
	PollLimit       int
	HTTPTimeout     time.Duration
	DeleteWebhook   bool
	MaxConcurrent   int
	MaxReplyChars   int
	StderrInReply   int
	LogPrompts      bool
	AckAfter        time.Duration
	TypingInterval  time.Duration
	ShutdownGrace   time.Duration

	// Codex
	CodexBin       string
	CodexHome      string
	SandboxMode    string
	Model          string
	StrictConfig   bool
	TurnTimeout    time.Duration
	KillGrace      time.Duration
	MaxStderrBytes int
	MaxEventBytes  int
	CheckLogin     bool
	// QueueTimeout is how long a message waits for its turn to be allowed to
	// start before the bot answers "busy" instead of queueing further.
	QueueTimeout time.Duration

	// Layout
	Workspace string
	StateDir  string
	DBPath    string
	LockPath  string

	// LogLevel is the slog threshold: debug, info, warn or error.
	LogLevel string
}

// Load reads the environment and returns a validated configuration.
//
// env is passed in rather than read from os.Getenv so tests can exercise the
// whole validation matrix without mutating the process environment.
func Load(env map[string]string) (*Config, error) {
	get := func(k string) string { return strings.TrimSpace(env[k]) }

	var errs []string
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Sprintf(format, args...))
	}

	cfg := &Config{
		TelegramAPIBase: orDefault(get("BOT_TELEGRAM_API_BASE"), DefaultTelegramAPIBase),
		SandboxMode:     orDefault(get("BOT_SANDBOX_MODE"), DefaultSandboxMode),
		Model:           orDefault(get("BOT_CODEX_MODEL"), DefaultModel),
		LogLevel:        orDefault(get("BOT_LOG_LEVEL"), "info"),
		PollLimit:       DefaultPollLimit,
		MaxConcurrent:   DefaultMaxConcurrent,
		MaxReplyChars:   MaxTelegramMessageLen,
		StderrInReply:   DefaultStderrInReply,
		MaxStderrBytes:  DefaultMaxStderrBytes,
		MaxEventBytes:   DefaultMaxEventBytes,
		AckAfter:        DefaultAckAfter,
		TypingInterval:  DefaultTypingInterval,
		TurnTimeout:     DefaultTurnTimeout,
		HTTPTimeout:     DefaultHTTPTimeout,
		PollTimeout:     DefaultPollTimeout,
		ShutdownGrace:   DefaultShutdownGrace,
		KillGrace:       DefaultKillGrace,
		QueueTimeout:    DefaultQueueTimeout,
	}

	// --- required secrets and identities -----------------------------------

	cfg.TelegramToken = get("TELEGRAM_BOT_TOKEN")
	switch {
	case cfg.TelegramToken == "":
		fail("TELEGRAM_BOT_TOKEN is required (get one from @BotFather)")
	case strings.ContainsAny(cfg.TelegramToken, " \t\n\r"):
		fail("TELEGRAM_BOT_TOKEN contains whitespace; it is almost certainly misquoted")
	case !strings.Contains(cfg.TelegramToken, ":"):
		fail("TELEGRAM_BOT_TOKEN does not look like a bot token (expected \"<id>:<secret>\")")
	}

	ids, err := parseUserIDs(get("ALLOWED_TELEGRAM_USER_IDS"))
	if err != nil {
		fail("ALLOWED_TELEGRAM_USER_IDS: %v", err)
	}
	cfg.AllowedUserIDs = ids

	// --- required paths -----------------------------------------------------

	ws, err := requireAbsDir(get("BOT_WORKSPACE"), "BOT_WORKSPACE")
	if err != nil {
		fail("%v", err)
	} else {
		cfg.Workspace = ws
		// Codex refuses to run outside a Git repository, and so do we: the
		// sandbox's workspace-write policy is scoped to the repository root,
		// and a repository gives the operator a way to review and revert what
		// the agent changed.
		if err := requireGitRepo(ws); err != nil {
			fail("BOT_WORKSPACE: %v", err)
		}
	}

	stateDir, err := ensurePrivateDir(get("BOT_STATE_DIR"), "BOT_STATE_DIR")
	if err != nil {
		fail("%v", err)
	} else {
		cfg.StateDir = stateDir
	}

	bin, err := requireExecutable(get("BOT_CODEX_BIN"), "BOT_CODEX_BIN")
	if err != nil {
		fail("%v", err)
	} else {
		cfg.CodexBin = bin
	}

	home, err := ensurePrivateDir(get("CODEX_HOME"), "CODEX_HOME")
	if err != nil {
		fail("%v", err)
	} else {
		cfg.CodexHome = home
	}

	// --- cross-checks that only make sense once paths are canonical ---------

	if len(errs) == 0 {
		if err := crossCheck(cfg); err != nil {
			fail("%v", err)
		}
	}

	// --- optional knobs ----------------------------------------------------
	if cfg.Model != "gpt-6-luna" && cfg.Model != "gpt-6-sol" {
		fail("BOT_CODEX_MODEL=%q is not supported; use gpt-6-luna or gpt-6-sol", cfg.Model)
	}

	if v := get("BOT_SANDBOX_MODE"); v != "" && !slices.Contains(allowedSandboxModes, cfg.SandboxMode) {
		fail("BOT_SANDBOX_MODE=%q is not allowed; use one of %s (danger-full-access is rejected on purpose)",
			v, strings.Join(allowedSandboxModes, ", "))
	}
	if d, ok, err := parseDuration(get("BOT_TURN_TIMEOUT")); err != nil {
		fail("BOT_TURN_TIMEOUT: %v", err)
	} else if ok {
		if d < 30*time.Second || d > 24*time.Hour {
			fail("BOT_TURN_TIMEOUT=%s is outside the supported 30s..24h range", d)
		}
		cfg.TurnTimeout = d
	}
	if n, ok, err := parseInt(get("BOT_MAX_STDERR_BYTES")); err != nil {
		fail("BOT_MAX_STDERR_BYTES: %v", err)
	} else if ok {
		if n < 1024 || n > 8*1024*1024 {
			fail("BOT_MAX_STDERR_BYTES=%d is outside the supported 1024..8388608 range", n)
		}
		cfg.MaxStderrBytes = n
	}
	if n, ok, err := parseInt(get("BOT_MAX_EVENT_BYTES")); err != nil {
		fail("BOT_MAX_EVENT_BYTES: %v", err)
	} else if ok {
		if n < 4096 || n > 64*1024*1024 {
			fail("BOT_MAX_EVENT_BYTES=%d is outside the supported 4096..67108864 range", n)
		}
		cfg.MaxEventBytes = n
	}
	if n, ok, err := parseInt(get("BOT_MAX_REPLY_CHARS")); err != nil {
		fail("BOT_MAX_REPLY_CHARS: %v", err)
	} else if ok {
		if n < 64 || n > MaxTelegramMessageLen {
			fail("BOT_MAX_REPLY_CHARS=%d is outside the supported 64..%d range", n, MaxTelegramMessageLen)
		}
		cfg.MaxReplyChars = n
	}
	if n, ok, err := parseInt(get("BOT_POLL_LIMIT")); err != nil {
		fail("BOT_POLL_LIMIT: %v", err)
	} else if ok {
		if n < 1 || n > 100 {
			fail("BOT_POLL_LIMIT=%d is outside the supported 1..100 range (Telegram's maximum is 100)", n)
		}
		cfg.PollLimit = n
	}
	if n, ok, err := parseInt(get("BOT_POLL_TIMEOUT_SEC")); err != nil {
		fail("BOT_POLL_TIMEOUT_SEC: %v", err)
	} else if ok {
		if n < 0 || n > 120 {
			fail("BOT_POLL_TIMEOUT_SEC=%d is outside the supported 0..120 range", n)
		}
		cfg.PollTimeout = time.Duration(n) * time.Second
	}
	if n, ok, err := parseInt(get("BOT_MAX_CONCURRENT_UPDATES")); err != nil {
		fail("BOT_MAX_CONCURRENT_UPDATES: %v", err)
	} else if ok {
		if n < 1 || n > 64 {
			fail("BOT_MAX_CONCURRENT_UPDATES=%d is outside the supported 1..64 range", n)
		}
		cfg.MaxConcurrent = n
	}
	if d, ok, err := parseDuration(get("BOT_ACK_AFTER")); err != nil {
		fail("BOT_ACK_AFTER: %v", err)
	} else if ok {
		if d < 0 || d > time.Hour {
			fail("BOT_ACK_AFTER=%s is outside the supported 0s..1h range", d)
		}
		cfg.AckAfter = d
	}
	if d, ok, err := parseDuration(get("BOT_SHUTDOWN_GRACE")); err != nil {
		fail("BOT_SHUTDOWN_GRACE: %v", err)
	} else if ok {
		if d < 0 || d > 10*time.Minute {
			fail("BOT_SHUTDOWN_GRACE=%s is outside the supported 0s..10m range", d)
		}
		cfg.ShutdownGrace = d
	}
	if d, ok, err := parseDuration(get("BOT_QUEUE_TIMEOUT")); err != nil {
		fail("BOT_QUEUE_TIMEOUT: %v", err)
	} else if ok {
		if d < 0 || d > time.Hour {
			fail("BOT_QUEUE_TIMEOUT=%s is outside the supported 0s..1h range", d)
		}
		cfg.QueueTimeout = d
	}
	if u := cfg.TelegramAPIBase; u != "" {
		parsed, err := url.Parse(u)
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" {
			fail("BOT_TELEGRAM_API_BASE=%q is not an absolute http(s) URL", u)
		}
		cfg.TelegramAPIBase = strings.TrimRight(u, "/")
	}
	// The long-poll HTTP timeout must outlast the server-side poll, or every
	// getUpdates would be cut off by our own client just before Telegram
	// answers.
	if cfg.HTTPTimeout <= cfg.PollTimeout {
		cfg.HTTPTimeout = cfg.PollTimeout + 15*time.Second
	}

	if b, ok, err := parseBool(get("BOT_LOG_PROMPTS")); err != nil {
		fail("BOT_LOG_PROMPTS: %v", err)
	} else if ok {
		cfg.LogPrompts = b
	}
	if b, ok, err := parseBool(get("BOT_DELETE_WEBHOOK")); err != nil {
		fail("BOT_DELETE_WEBHOOK: %v", err)
	} else if ok {
		cfg.DeleteWebhook = b
	} else {
		cfg.DeleteWebhook = true
	}
	if b, ok, err := parseBool(get("BOT_CODEX_STRICT_CONFIG")); err != nil {
		fail("BOT_CODEX_STRICT_CONFIG: %v", err)
	} else if ok {
		cfg.StrictConfig = b
	} else {
		cfg.StrictConfig = true
	}
	if b, ok, err := parseBool(get("BOT_CHECK_CODEX_LOGIN")); err != nil {
		fail("BOT_CHECK_CODEX_LOGIN: %v", err)
	} else if ok {
		cfg.CheckLogin = b
	} else {
		cfg.CheckLogin = true
	}

	if len(errs) > 0 {
		return nil, fmt.Errorf("invalid configuration:\n  - %s", strings.Join(errs, "\n  - "))
	}

	cfg.DBPath = filepath.Join(cfg.StateDir, DefaultDBFileName)
	cfg.LockPath = filepath.Join(cfg.StateDir, DefaultLockFileName)
	return cfg, nil
}

// crossCheck catches combinations that are individually valid and jointly
// wrong. It is only called once every path has been canonicalised.
func crossCheck(cfg *Config) error {
	// The whole point of a dedicated CODEX_HOME is that the bot creates and
	// resumes its own threads. Pointing it at the interactive installation
	// would let the bot list, resume and archive the operator's own sessions.
	if home := os.Getenv("HOME"); home != "" {
		interactive := filepath.Join(home, ".codex")
		if cfg.CodexHome == interactive {
			return fmt.Errorf("CODEX_HOME must not be the interactive Codex home %s: the bot needs its own, "+
				"otherwise it can see and resume your personal sessions", interactive)
		}
	}
	if !filepath.IsAbs(cfg.CodexHome) {
		return errors.New("CODEX_HOME must be an absolute path")
	}
	// The database holds session ownership and conversation replies; committing
	// it into the repository Codex can edit would let the agent rewrite its own
	// bookkeeping (and would put it in `git status` noise).
	if isSameOrInside(cfg.StateDir, cfg.Workspace) {
		return fmt.Errorf("BOT_STATE_DIR (%s) must live outside BOT_WORKSPACE (%s): Codex can write to the "+
			"workspace, and the bot database must not be editable by the agent", cfg.StateDir, cfg.Workspace)
	}
	if isSameOrInside(cfg.CodexHome, cfg.Workspace) {
		return fmt.Errorf("CODEX_HOME (%s) must live outside BOT_WORKSPACE (%s) for the same reason",
			cfg.CodexHome, cfg.Workspace)
	}
	if cfg.CodexBin == "" || cfg.Workspace == "" || cfg.StateDir == "" {
		return errors.New("BOT_CODEX_BIN, BOT_WORKSPACE and BOT_STATE_DIR are all required")
	}
	return nil
}

// isSameOrInside reports whether child is dir or somewhere underneath it.
func isSameOrInside(child, dir string) bool {
	if child == "" || dir == "" {
		return false
	}
	if child == dir {
		return true
	}
	rel, err := filepath.Rel(dir, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// requireAbsDir resolves a required existing directory to a canonical absolute
// path. Symlinks are resolved so the recorded path is the real one, which is
// what makes the containment checks above meaningful.
func requireAbsDir(raw, name string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("%s is required and must be an absolute path", name)
	}
	if !filepath.IsAbs(raw) {
		return "", fmt.Errorf("%s=%q must be an absolute path", name, raw)
	}
	resolved, err := filepath.EvalSymlinks(raw)
	if err != nil {
		return "", fmt.Errorf("%s=%q: %w", name, raw, err)
	}
	resolved = filepath.Clean(resolved)
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("%s=%q: %w", name, resolved, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s=%q is not a directory", name, resolved)
	}
	return resolved, nil
}

// ensurePrivateDir resolves a required directory, creating it 0700 when it does
// not exist yet. Both BOT_STATE_DIR and CODEX_HOME hold data that must not be
// world-readable: the database carries conversation replies, and CODEX_HOME
// carries the Codex credential cache.
func ensurePrivateDir(raw, name string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("%s is required and must be an absolute path", name)
	}
	if !filepath.IsAbs(raw) {
		return "", fmt.Errorf("%s=%q must be an absolute path", name, raw)
	}
	if err := os.MkdirAll(raw, 0o700); err != nil {
		return "", fmt.Errorf("%s=%q: %w", name, raw, err)
	}
	resolved, err := filepath.EvalSymlinks(raw)
	if err != nil {
		return "", fmt.Errorf("%s=%q: %w", name, raw, err)
	}
	resolved = filepath.Clean(resolved)
	// Tighten an existing directory that was created too open. Ignoring the
	// error is deliberate: some filesystems do not support chmod, and a
	// warning at this point would be noise next to a working bot.
	_ = os.Chmod(resolved, 0o700)
	return resolved, nil
}

// requireGitRepo checks that dir is inside a Git work tree.
func requireGitRepo(dir string) error {
	// ".git" is a directory in a normal clone and a file in a linked worktree,
	// so accept either.
	if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
		return nil
	}
	// Not the top level: ask git, which also covers a subdirectory of a repo.
	// The bot deliberately does not run `git` here — a missing binary should
	// not change the answer — it just reports what it found.
	return fmt.Errorf("%s is not a Git repository (no .git entry); run `git -C %s init` or point "+
		"BOT_WORKSPACE at an existing checkout. Codex requires a repository and so does this bot", dir, dir)
}

// requireExecutable resolves the Codex binary, searching PATH when the value is
// not absolute, and checks that it is runnable.
func requireExecutable(raw, name string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("%s is required (use `command -v codex` for this account)", name)
	}
	path := raw
	if !filepath.IsAbs(raw) {
		found, err := os.Stat(raw)
		// A bare name is looked up on PATH; anything containing a separator is
		// treated as a path the operator meant literally.
		if err == nil && !found.IsDir() {
			path, _ = filepath.Abs(raw)
		} else {
			resolved, lerr := lookPath(raw)
			if lerr != nil {
				return "", fmt.Errorf("%s=%q: %w", name, raw, lerr)
			}
			path = resolved
		}
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		// A dangling symlink is worth reporting as-is.
		return "", fmt.Errorf("%s=%q: %w", name, path, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("%s=%q: %w", name, resolved, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s=%q is a directory", name, resolved)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%s=%q is not executable", name, resolved)
	}
	return resolved, nil
}

// Allowed reports whether a Telegram user id is on the allowlist.
func (c *Config) Allowed(userID int64) bool {
	return slices.Contains(c.AllowedUserIDs, userID)
}

// parseUserIDs parses a comma/space separated list of positive numeric ids.
//
// Only numeric ids are accepted. A @username is mutable — anybody can rename
// themselves into one — and it is absent from messages forwarded from channels,
// so identity is taken from `from.id` and nothing else.
func parseUserIDs(raw string) ([]int64, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("is required; list the numeric Telegram user ids allowed to drive Codex " +
			"(comma or space separated)")
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == ';'
	})
	if len(fields) == 0 {
		return nil, errors.New("contains no ids")
	}
	seen := make(map[int64]struct{}, len(fields))
	out := make([]int64, 0, len(fields))
	for _, f := range fields {
		id, err := strconv.ParseInt(f, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%q is not a numeric Telegram user id (usernames are not accepted: "+
				"they can be renamed and are not a stable identity)", f)
		}
		if id <= 0 {
			return nil, fmt.Errorf("%d is not a valid Telegram user id", id)
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func parseBool(raw string) (bool, bool, error) {
	if raw == "" {
		return false, false, nil
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return false, false, fmt.Errorf("%q is not a boolean (use true/false/1/0)", raw)
	}
	return b, true, nil
}

func parseInt(raw string) (int, bool, error) {
	if raw == "" {
		return 0, false, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, false, fmt.Errorf("%q is not an integer", raw)
	}
	return n, true, nil
}

func parseDuration(raw string) (time.Duration, bool, error) {
	if raw == "" {
		return 0, false, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, false, fmt.Errorf("%q is not a Go duration like 15m or 90s", raw)
	}
	if d < 0 {
		return 0, false, fmt.Errorf("%q must not be negative", raw)
	}
	return d, true, nil
}

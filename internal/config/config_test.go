package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-telegram-bot/internal/testkit"
)

// baseEnv is a configuration that Load accepts. Each test overrides one key so a
// failure names the rule being checked.
func baseEnv(t *testing.T) map[string]string {
	t.Helper()
	ws := testkit.Workspace(t)
	bin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho codex-cli 0.0.0-test\n"), 0o700); err != nil {
		t.Fatalf("write a fake codex: %v", err)
	}
	return map[string]string{
		"TELEGRAM_BOT_TOKEN":        "123456:AA test-token-value",
		"ALLOWED_TELEGRAM_USER_IDS": "111,222",
		"BOT_WORKSPACE":             ws,
		"BOT_STATE_DIR":             testkit.StateDir(t),
		"BOT_CODEX_BIN":             bin,
		"CODEX_HOME":                testkit.CodexHome(t),
	}
}

// fixToken removes the deliberate space so token-shape tests can vary one thing.
func withValidToken(env map[string]string) map[string]string {
	env["TELEGRAM_BOT_TOKEN"] = "123456:AAtest-token-value"
	return env
}

func TestLoadAcceptsAValidEnvironment(t *testing.T) {
	cfg, err := Load(withValidToken(baseEnv(t)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SandboxMode != DefaultSandboxMode {
		t.Errorf("SandboxMode = %q, want %q", cfg.SandboxMode, DefaultSandboxMode)
	}
	if cfg.TurnTimeout != DefaultTurnTimeout {
		t.Errorf("TurnTimeout = %s, want %s", cfg.TurnTimeout, DefaultTurnTimeout)
	}
	if !cfg.StrictConfig {
		t.Error("StrictConfig should default to true")
	}
	if !cfg.DeleteWebhook {
		t.Error("DeleteWebhook should default to true")
	}
	if !cfg.CheckLogin {
		t.Error("CheckLogin should default to true")
	}
	if cfg.LogPrompts {
		t.Error("LogPrompts must default to false: prompts are not logged unless asked for")
	}
	if cfg.QueueTimeout != DefaultQueueTimeout {
		t.Errorf("QueueTimeout = %s, want %s", cfg.QueueTimeout, DefaultQueueTimeout)
	}
	if cfg.MaxReplyChars != MaxTelegramMessageLen {
		t.Errorf("MaxReplyChars = %d, want %d", cfg.MaxReplyChars, MaxTelegramMessageLen)
	}
	if cfg.DBPath != filepath.Join(cfg.StateDir, DefaultDBFileName) {
		t.Errorf("DBPath = %q, want it inside the state dir", cfg.DBPath)
	}
	if cfg.LockPath == "" {
		t.Error("LockPath is empty")
	}
	if !cfg.Allowed(111) || !cfg.Allowed(222) {
		t.Errorf("AllowedUserIDs = %v does not contain both ids", cfg.AllowedUserIDs)
	}
	if cfg.Allowed(333) {
		t.Error("Allowed(333) = true for an id that is not on the list")
	}
}

func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	_, err := Load(map[string]string{})
	if err == nil {
		t.Fatal("Load accepted an empty environment")
	}
	msg := err.Error()
	for _, want := range []string{
		"TELEGRAM_BOT_TOKEN", "ALLOWED_TELEGRAM_USER_IDS",
		"BOT_WORKSPACE", "BOT_STATE_DIR", "BOT_CODEX_BIN", "CODEX_HOME",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error does not mention %s:\n%s", want, msg)
		}
	}
}

func TestLoadRejectsMissingRequiredValues(t *testing.T) {
	for _, key := range []string{
		"TELEGRAM_BOT_TOKEN", "ALLOWED_TELEGRAM_USER_IDS",
		"BOT_WORKSPACE", "BOT_STATE_DIR", "BOT_CODEX_BIN", "CODEX_HOME",
	} {
		t.Run(key, func(t *testing.T) {
			env := withValidToken(baseEnv(t))
			delete(env, key)
			_, err := Load(env)
			if err == nil || !strings.Contains(err.Error(), key) {
				t.Fatalf("Load accepted a missing %s: %v", key, err)
			}
		})
	}
}

func TestLoadRejectsRelativePaths(t *testing.T) {
	for _, key := range []string{"BOT_WORKSPACE", "BOT_STATE_DIR", "CODEX_HOME"} {
		t.Run(key, func(t *testing.T) {
			env := withValidToken(baseEnv(t))
			env[key] = "relative/path"
			_, err := Load(env)
			if err == nil || !strings.Contains(err.Error(), "absolute") {
				t.Fatalf("Load accepted a relative %s: %v", key, err)
			}
		})
	}
}

func TestLoadRejectsAWorkspaceThatIsNotARepository(t *testing.T) {
	env := withValidToken(baseEnv(t))
	env["BOT_WORKSPACE"] = t.TempDir() // no .git
	_, err := Load(env)
	if err == nil {
		t.Fatal("Load accepted a workspace that is not a Git repository")
	}
	if !strings.Contains(err.Error(), "Git repository") {
		t.Errorf("the error does not explain the requirement: %v", err)
	}
}

func TestLoadRejectsTheInteractiveCodexHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	interactive := filepath.Join(home, ".codex")
	if err := os.MkdirAll(interactive, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	env := withValidToken(baseEnv(t))
	env["CODEX_HOME"] = interactive

	_, err := Load(env)
	if err == nil {
		t.Fatal("Load accepted the interactive Codex home; the bot would see the operator's own sessions")
	}
	if !strings.Contains(err.Error(), "interactive Codex home") {
		t.Errorf("the error does not explain why: %v", err)
	}
}

func TestLoadRejectsStateInsideTheWorkspace(t *testing.T) {
	env := withValidToken(baseEnv(t))
	inside := filepath.Join(env["BOT_WORKSPACE"], "state")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	env["BOT_STATE_DIR"] = inside

	_, err := Load(env)
	if err == nil {
		t.Fatal("Load accepted a state directory inside the workspace Codex can write to")
	}
	if !strings.Contains(err.Error(), "outside BOT_WORKSPACE") {
		t.Errorf("the error does not explain why: %v", err)
	}
}

func TestLoadRejectsCodexHomeInsideTheWorkspace(t *testing.T) {
	env := withValidToken(baseEnv(t))
	inside := filepath.Join(env["BOT_WORKSPACE"], "codex-home")
	if err := os.MkdirAll(inside, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	env["CODEX_HOME"] = inside

	if _, err := Load(env); err == nil {
		t.Fatal("Load accepted a CODEX_HOME inside the workspace")
	}
}

func TestLoadRejectsDangerFullAccess(t *testing.T) {
	env := withValidToken(baseEnv(t))
	env["BOT_SANDBOX_MODE"] = "danger-full-access"
	_, err := Load(env)
	if err == nil {
		t.Fatal("Load accepted danger-full-access")
	}
	if !strings.Contains(err.Error(), "rejected on purpose") {
		t.Errorf("the error should say the mode is refused deliberately: %v", err)
	}
}

func TestLoadAcceptsReadOnlySandbox(t *testing.T) {
	env := withValidToken(baseEnv(t))
	env["BOT_SANDBOX_MODE"] = "read-only"
	cfg, err := Load(env)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.SandboxMode != "read-only" {
		t.Errorf("SandboxMode = %q", cfg.SandboxMode)
	}
}

func TestParseUserIDs(t *testing.T) {
	ok := []struct {
		in   string
		want []int64
	}{
		{"123", []int64{123}},
		{"123,456", []int64{123, 456}},
		{"123 456\t789", []int64{123, 456, 789}},
		{"123, 456 ; 789\n", []int64{123, 456, 789}},
		{"123,123", []int64{123}}, // duplicates collapse
	}
	for _, c := range ok {
		got, err := parseUserIDs(c.in)
		if err != nil {
			t.Errorf("parseUserIDs(%q): %v", c.in, err)
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("parseUserIDs(%q) = %v, want %v", c.in, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("parseUserIDs(%q) = %v, want %v", c.in, got, c.want)
				break
			}
		}
	}

	bad := []string{
		"",
		"   ",
		"@username", // a username is not an identity
		"somebody",
		"123,notanumber",
		"0", // not a valid user id
		"-5",
		"99999999999999999999999", // overflows int64
	}
	for _, in := range bad {
		if got, err := parseUserIDs(in); err == nil {
			t.Errorf("parseUserIDs(%q) = %v, want an error", in, got)
		}
	}
}

func TestLoadRejectsBadTokenShapes(t *testing.T) {
	cases := map[string]string{
		"whitespace": "123456:AA has a space",
		"newline":    "123456:AA\nsecret",
		"nocolon":    "just-a-string",
	}
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			env := baseEnv(t)
			env["TELEGRAM_BOT_TOKEN"] = tok
			if _, err := Load(env); err == nil {
				t.Fatalf("Load accepted the token %q", tok)
			}
		})
	}
}

func TestLoadRejectsOutOfRangeKnobs(t *testing.T) {
	cases := []struct{ key, value string }{
		{"BOT_TURN_TIMEOUT", "5s"},  // below the 30s floor
		{"BOT_TURN_TIMEOUT", "48h"}, // above the 24h ceiling
		{"BOT_TURN_TIMEOUT", "notaduration"},
		{"BOT_POLL_LIMIT", "0"},
		{"BOT_POLL_LIMIT", "101"},       // Telegram's maximum is 100
		{"BOT_MAX_REPLY_CHARS", "4097"}, // above Telegram's limit
		{"BOT_MAX_REPLY_CHARS", "10"},
		{"BOT_MAX_CONCURRENT_UPDATES", "0"},
		{"BOT_MAX_STDERR_BYTES", "1"},
		{"BOT_MAX_EVENT_BYTES", "10"},
		{"BOT_LOG_PROMPTS", "maybe"},
		{"BOT_TELEGRAM_API_BASE", "not a url"},
		{"BOT_QUEUE_TIMEOUT", "-1s"},
	}
	for _, c := range cases {
		t.Run(c.key+"="+c.value, func(t *testing.T) {
			env := withValidToken(baseEnv(t))
			env[c.key] = c.value
			if _, err := Load(env); err == nil {
				t.Fatalf("Load accepted %s=%s", c.key, c.value)
			}
		})
	}
}

func TestLoadAcceptsAndAppliesKnobs(t *testing.T) {
	env := withValidToken(baseEnv(t))
	env["BOT_TURN_TIMEOUT"] = "45m"
	env["BOT_POLL_LIMIT"] = "10"
	env["BOT_POLL_TIMEOUT_SEC"] = "5"
	env["BOT_MAX_REPLY_CHARS"] = "1000"
	env["BOT_LOG_PROMPTS"] = "true"
	env["BOT_DELETE_WEBHOOK"] = "false"
	env["BOT_CODEX_STRICT_CONFIG"] = "false"
	env["BOT_CHECK_CODEX_LOGIN"] = "false"
	env["BOT_QUEUE_TIMEOUT"] = "90s"
	env["BOT_CODEX_MODEL"] = "gpt-5-codex"
	env["BOT_TELEGRAM_API_BASE"] = "http://127.0.0.1:1/"

	cfg, err := Load(env)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TurnTimeout != 45*time.Minute {
		t.Errorf("TurnTimeout = %s", cfg.TurnTimeout)
	}
	if cfg.PollLimit != 10 {
		t.Errorf("PollLimit = %d", cfg.PollLimit)
	}
	if cfg.PollTimeout != 5*time.Second {
		t.Errorf("PollTimeout = %s", cfg.PollTimeout)
	}
	// The HTTP timeout must outlast the long poll, or the client would cut off
	// every request just before Telegram answered.
	if cfg.HTTPTimeout <= cfg.PollTimeout {
		t.Errorf("HTTPTimeout %s must exceed PollTimeout %s", cfg.HTTPTimeout, cfg.PollTimeout)
	}
	if cfg.MaxReplyChars != 1000 || !cfg.LogPrompts || cfg.DeleteWebhook || cfg.StrictConfig || cfg.CheckLogin {
		t.Errorf("knobs were not applied: %+v", cfg)
	}
	if cfg.QueueTimeout != 90*time.Second {
		t.Errorf("QueueTimeout = %s", cfg.QueueTimeout)
	}
	if cfg.Model != "gpt-5-codex" {
		t.Errorf("Model = %q", cfg.Model)
	}
	if cfg.TelegramAPIBase != "http://127.0.0.1:1" {
		t.Errorf("TelegramAPIBase = %q, want the trailing slash trimmed", cfg.TelegramAPIBase)
	}
}

func TestLoadCreatesPrivateDirectories(t *testing.T) {
	env := withValidToken(baseEnv(t))
	state := filepath.Join(t.TempDir(), "nested", "state")
	home := filepath.Join(t.TempDir(), "nested", "codex")
	env["BOT_STATE_DIR"] = state
	env["CODEX_HOME"] = home

	cfg, err := Load(env)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, dir := range []string{cfg.StateDir, cfg.CodexHome} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatalf("stat %s: %v", dir, err)
		}
		if perm := info.Mode().Perm(); perm != 0o700 {
			t.Errorf("%s has mode %o, want 0700: it holds the database and the credential cache", dir, perm)
		}
	}
}

func TestLoadTightensAnAlreadyOpenStateDir(t *testing.T) {
	env := withValidToken(baseEnv(t))
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	env["BOT_STATE_DIR"] = dir
	if _, err := Load(env); err != nil {
		t.Fatalf("Load: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("the state directory is still group/world accessible: %o", perm)
	}
}

func TestRequireExecutableSearchesPath(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "my-codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Setenv("PATH", dir)

	got, err := requireExecutable("my-codex", "BOT_CODEX_BIN")
	if err != nil {
		t.Fatalf("requireExecutable: %v", err)
	}
	if got != bin {
		t.Errorf("resolved to %q, want %q", got, bin)
	}
	if _, err := requireExecutable("definitely-not-installed-xyz", "BOT_CODEX_BIN"); err == nil {
		t.Error("requireExecutable accepted a command that is not on PATH")
	}
}

func TestRequireExecutableRejectsANonExecutable(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(bin, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := requireExecutable(bin, "BOT_CODEX_BIN"); err == nil {
		t.Fatal("requireExecutable accepted a non-executable file")
	}
}

func TestIsSameOrInside(t *testing.T) {
	cases := []struct {
		child, dir string
		want       bool
	}{
		{"/a/b", "/a", true},
		{"/a", "/a", true},
		{"/a/b/c", "/a/b", true},
		{"/ab", "/a", false},
		{"/b", "/a", false},
		{"/a", "/a/b", false},
		{"", "/a", false},
		{"/a", "", false},
	}
	for _, c := range cases {
		if got := isSameOrInside(c.child, c.dir); got != c.want {
			t.Errorf("isSameOrInside(%q, %q) = %v, want %v", c.child, c.dir, got, c.want)
		}
	}
}

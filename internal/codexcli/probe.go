package codexcli

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// probeTimeout bounds the startup probes. They run before the first getUpdates,
// so a hung codex must not hang the service forever.
const probeTimeout = 30 * time.Second

// probeOutput caps what a probe may print.
const probeOutput = 8 * 1024

// Version runs `codex --version` and returns its output, e.g. "codex-cli 0.156.1".
//
// This is the startup check that the configured BOT_CODEX_BIN is really the
// Codex CLI and really runnable by this account. A path that exists but is a
// shell wrapper around a missing binary fails here rather than at the first
// Telegram message.
func (r *Runner) Version(ctx context.Context) (string, error) {
	out, err := r.probe(ctx, "--version")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

// LoginStatus reports whether CODEX_HOME holds usable Codex credentials.
//
// `codex login status` exits 0 whether or not the account is logged in — only
// its output differs ("Logged in using ChatGPT" versus "Not logged in") — so the
// text is what has to be inspected. Verified on codex-cli 0.156.1.
//
// The returned string is Codex's own first line, which is safe to log: it names
// the login method, never a token.
func (r *Runner) LoginStatus(ctx context.Context) (loggedIn bool, detail string, err error) {
	out, err := r.probe(ctx, "login", "status")
	if err != nil {
		return false, "", err
	}
	trimmed := strings.TrimSpace(out)
	first := trimmed
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = strings.TrimSpace(first[:i])
	}
	if first == "" {
		first = "<no output>"
	}
	lower := strings.ToLower(trimmed)
	switch {
	case strings.Contains(lower, "not logged in"):
		return false, first, nil
	case strings.Contains(lower, "logged in"):
		return true, first, nil
	default:
		// Unknown wording from a future release: do not claim either way.
		return false, first + " (unrecognised login status output)", nil
	}
}

// probe runs codex with the given arguments, the runner's environment and a
// bounded timeout, and returns combined output.
func (r *Runner) probe(ctx context.Context, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, r.cfg.Bin, args...)
	cmd.Dir = r.cfg.Workspace
	cmd.Env = r.childEnv()
	cmd.Stdin = nil

	// A bounded buffer: a probe that prints megabytes must not grow the bot's
	// memory, and the tail is the interesting part anyway.
	tail := newTailBuffer(probeOutput)
	cmd.Stdout = tail
	cmd.Stderr = tail

	if err := cmd.Run(); err != nil {
		return tail.String(), fmt.Errorf("codexcli: `codex %s` failed: %w (output: %s)",
			strings.Join(args, " "), err, strings.TrimSpace(tail.String()))
	}
	return tail.String(), nil
}

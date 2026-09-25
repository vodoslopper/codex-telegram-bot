// Package codexcli runs the Codex CLI as a child process and turns its JSONL
// output into a single agent reply.
//
// One exported operation matters: Runner.Run, which executes exactly one turn —
// either a brand new `codex exec` or a `codex exec resume <thread-id>` — and
// reports the final agent message, the thread id, and what went wrong if
// anything did.
//
// Three rules shape this package:
//
//   - The prompt travels as a single argv element after a `--` terminator, and
//     the child is spawned with exec.CommandContext and an argument array. There
//     is no shell anywhere in this path, so a message like
//     `; rm -rf ~ #` is a string Codex reads, never a command line.
//   - The child gets its own process group (Setpgid) and cancellation kills the
//     group, because Codex spawns a shell, which spawns the agent's commands.
//     Killing only the direct child would orphan those.
//   - Success is decided by the event stream, not by the exit code alone: only
//     `turn.completed` with a final `agent_message` counts. A zero exit with a
//     truncated stream is a failure, and a nonzero exit with a complete stream
//     still is one.
//
// The flag choices below were verified against codex-cli 0.156.1 on Linux. See
// README.md for the exact argv and for how to re-verify it.
package codexcli

import (
	"fmt"
	"strings"
)

// Sandbox modes this package knows how to request.
const (
	SandboxReadOnly       = "read-only"
	SandboxWorkspaceWrite = "workspace-write"
)

// Request describes one turn.
type Request struct {
	// ThreadID is the exact UUID stored for the session. When empty, Run starts
	// a new Codex thread and reports the id it was given.
	ThreadID string
	// Prompt is the user's text. It is passed as one argv element.
	Prompt string
}

// IsResume reports whether this request continues an existing thread.
func (r Request) IsResume() bool { return r.ThreadID != "" }

// validate rejects requests that would produce a malformed command line.
func (r Request) validate() error {
	if strings.TrimSpace(r.Prompt) == "" {
		return fmt.Errorf("codexcli: empty prompt")
	}
	// A UUID is 36 characters of hex and dashes. Anything else is a thread
	// *name*, which Codex also accepts — and which is exactly the ambiguity
	// this bot must not have, because a name could match a different session.
	if r.ThreadID != "" && !isUUID(r.ThreadID) {
		return fmt.Errorf("codexcli: stored thread id %q is not a UUID; refusing to resume by name", r.ThreadID)
	}
	return nil
}

// isUUID reports whether s has the 8-4-4-4-12 hexadecimal shape.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !isHexDigit(byte(r)) {
				return false
			}
		}
	}
	return true
}

func isHexDigit(b byte) bool {
	return (b >= '0' && b <= '9') || (b >= 'a' && b <= 'f') || (b >= 'A' && b <= 'F')
}

// Argv returns the exact argument list Run will use, prompt included.
//
// New turn:
//
//	codex exec --json [--strict-config] [--sandbox workspace-write] [-m MODEL] -- <PROMPT>
//
// Resumed turn:
//
//	codex exec resume <THREAD_ID> --json [--strict-config] -c sandbox_mode="workspace-write" [-m MODEL] -- <PROMPT>
//
// Why the two differ:
//
//   - `codex exec resume` in 0.156.1 has no --sandbox/-s flag at all (it exits
//     2 with "unexpected argument '-s' found"), so the sandbox policy for a
//     resumed turn is set through the documented config key instead:
//     `-c sandbox_mode="workspace-write"`. The value is quoted so it parses as
//     TOML rather than falling back to the raw-string path.
//   - `codex exec resume` also has no --cd/-C flag, and it resolves the session
//     to resume relative to the process working directory (Codex filters
//     recorded sessions by cwd unless --all is given). The working directory is
//     therefore set on the child process itself (cmd.Dir) for both paths, which
//     is the one mechanism that works for both. --all is never used: it would
//     widen the search across every session in CODEX_HOME.
//   - `--last` is never used for the same reason. The bot resumes only the UUID
//     it recorded from that session's own thread.started event.
//   - `--ephemeral` is never used: threads must survive a bot restart.
//   - `--` terminates option parsing, so a prompt that begins with "-" is still
//     a prompt. Verified: `codex exec --json -- "-- -x"` does not error out.
func (r *Runner) Argv(req Request) ([]string, error) {
	if err := req.validate(); err != nil {
		return nil, err
	}
	var a []string
	if req.IsResume() {
		a = append(a, "exec", "resume", req.ThreadID, "--json")
	} else {
		a = append(a, "exec", "--json")
	}
	if r.cfg.StrictConfig {
		a = append(a, "--strict-config")
	}
	if req.IsResume() {
		// No -s on resume; set the policy through the config key instead.
		a = append(a, "-c", fmt.Sprintf("sandbox_mode=%q", r.cfg.Sandbox))
	} else {
		a = append(a, "--sandbox", r.cfg.Sandbox)
	}
	if r.cfg.Model != "" {
		a = append(a, "-m", r.cfg.Model)
	}
	return append(a, "--", req.Prompt), nil
}

// ElidedArgv is Argv with the prompt replaced by a placeholder, for storing and
// logging. The exact command line is genuinely useful when debugging a failed
// turn; the user's prompt is not the bot's business to persist.
func (r *Runner) ElidedArgv(req Request) []string {
	a, err := r.Argv(req)
	if err != nil {
		return []string{"<invalid request>"}
	}
	out := make([]string, len(a))
	copy(out, a)
	out[len(out)-1] = "<prompt>"
	return out
}

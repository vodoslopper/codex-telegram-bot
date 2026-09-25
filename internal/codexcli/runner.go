package codexcli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Errors reported by Run. Callers map these to user-visible text; the distinction
// matters because each one has a different remedy.
var (
	// ErrTimeout means BOT_TURN_TIMEOUT elapsed and the process group was killed.
	ErrTimeout = errors.New("codex turn timed out")
	// ErrCancelled means the caller's context was cancelled (the user sent /stop).
	ErrCancelled = errors.New("codex turn cancelled")
	// ErrExited means the CLI exited nonzero without explaining itself in the
	// event stream.
	ErrExited = errors.New("codex exited with an error")
	// ErrTurnFailed means Codex emitted turn.failed.
	ErrTurnFailed = errors.New("codex reported turn.failed")
	// ErrNoThreadID means no thread.started event arrived, so there is no thread
	// to resume and the session stays threadless.
	ErrNoThreadID = errors.New("codex did not report a thread id")
	// ErrIncompleteStream means the output ended without turn.completed.
	ErrIncompleteStream = errors.New("codex output ended before turn.completed")
	// ErrNoFinalReply means the turn completed but carried no agent message.
	ErrNoFinalReply = errors.New("codex completed the turn without a final message")
)

// Config is everything the runner needs. All paths are absolute; internal/config
// guarantees that.
type Config struct {
	// Bin is the resolved codex executable.
	Bin string
	// Workspace is the Git repository Codex runs in. It becomes the child's
	// working directory, which is also how `codex exec resume` finds the thread.
	Workspace string
	// CodexHome is the dedicated CODEX_HOME for the child process.
	CodexHome string
	// Sandbox is "workspace-write" or "read-only".
	Sandbox string
	// Model is an optional -m value.
	Model string
	// StrictConfig passes --strict-config, which makes Codex fail loudly on an
	// unrecognised config key instead of silently ignoring the sandbox override.
	StrictConfig bool

	// Timeout bounds one turn.
	Timeout time.Duration
	// KillGrace is how long the process group gets between SIGTERM and SIGKILL.
	KillGrace time.Duration
	// MaxStderrBytes caps the retained stderr tail.
	MaxStderrBytes int
	// MaxEventBytes caps one JSONL line; longer lines are skipped, not fatal.
	MaxEventBytes int

	// BaseEnv is the environment to start from. nil means os.Environ().
	BaseEnv []string
}

func (c Config) withDefaults() Config {
	if c.Sandbox == "" {
		c.Sandbox = SandboxWorkspaceWrite
	}
	if c.Timeout <= 0 {
		c.Timeout = 15 * time.Minute
	}
	if c.KillGrace <= 0 {
		c.KillGrace = 5 * time.Second
	}
	if c.MaxStderrBytes <= 0 {
		c.MaxStderrBytes = 64 * 1024
	}
	if c.MaxEventBytes <= 0 {
		c.MaxEventBytes = 8 * 1024 * 1024
	}
	return c
}

// Result is what one turn produced. It is always returned, even on failure,
// because a failed turn can still have learned the thread id and that id is
// worth persisting.
type Result struct {
	ThreadID string
	Reply    string
	Usage    Usage

	ExitCode  int
	Stderr    string
	Argv      []string // prompt elided
	Duration  time.Duration
	TimedOut  bool
	Cancelled bool
	Events    Accumulator
}

// Runner executes Codex turns. It is safe for concurrent use; each Run is
// independent, and the caller is responsible for not running two turns in the
// same worktree at once (internal/bot does that with a workspace lock).
type Runner struct {
	cfg Config
	log *slog.Logger
}

// NewRunner validates its configuration and returns a runner.
func NewRunner(cfg Config, log *slog.Logger) (*Runner, error) {
	cfg = cfg.withDefaults()
	if cfg.Bin == "" {
		return nil, errors.New("codexcli: Bin is required")
	}
	if cfg.Workspace == "" {
		return nil, errors.New("codexcli: Workspace is required")
	}
	if cfg.CodexHome == "" {
		return nil, errors.New("codexcli: CodexHome is required")
	}
	switch cfg.Sandbox {
	case SandboxReadOnly, SandboxWorkspaceWrite:
	default:
		return nil, fmt.Errorf("codexcli: sandbox %q is not supported (danger-full-access is refused on purpose)", cfg.Sandbox)
	}
	if log == nil {
		log = slog.Default()
	}
	return &Runner{cfg: cfg, log: log}, nil
}

// Sandbox is the policy turns run under.
func (r *Runner) Sandbox() string { return r.cfg.Sandbox }

// Run executes one turn and waits for it.
//
// The child gets its own process group. When ctx is done the group receives
// SIGTERM, and SIGKILL KillGrace later if it is still alive, so the shells and
// build processes Codex started do not outlive the turn.
func (r *Runner) Run(ctx context.Context, req Request) (*Result, error) {
	argv, err := r.Argv(req)
	if err != nil {
		return nil, err
	}

	runCtx, cancel := context.WithTimeout(ctx, r.cfg.Timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, r.cfg.Bin, argv...)
	cmd.Dir = r.cfg.Workspace
	cmd.Env = r.childEnv()
	// Codex reads stdin even when a prompt argument is given, and appends any
	// piped content as a <stdin> block. /dev/null gives it an immediate EOF so a
	// Telegram message is the whole prompt and nothing else.
	cmd.Stdin = nil

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("codexcli: stdout pipe: %w", err)
	}
	tail := newTailBuffer(r.cfg.MaxStderrBytes)
	cmd.Stderr = tail

	// Put the child in its own process group so -pid addresses the whole tree.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Cancellation is handled by the watchdog below, which signals the group
	// rather than the single child. Returning nil here keeps Go from killing
	// just the parent and orphaning its children.
	cmd.Cancel = func() error { return nil }
	cmd.WaitDelay = r.cfg.KillGrace + 10*time.Second

	started := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("codexcli: start %s: %w", r.cfg.Bin, err)
	}

	procDone := make(chan struct{})
	go r.watchdog(runCtx, cmd.Process.Pid, procDone)

	acc := &Accumulator{}
	lr := newLineReader(stdout, r.cfg.MaxEventBytes)
	for {
		line, oversized, rerr := lr.Next()
		if oversized {
			acc.OversizedLines++
			r.log.Warn("skipped an oversized codex JSONL line", "limit", r.cfg.MaxEventBytes)
		} else {
			acc.Handle(line)
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			// A read error mid-stream usually means the pipe was closed by
			// WaitDelay after a cancellation. Stop and let Wait classify it.
			r.log.Debug("codex stdout read stopped early", "error", rerr.Error())
			break
		}
	}

	waitErr := cmd.Wait()
	close(procDone)

	res := &Result{
		ThreadID: acc.ThreadID,
		Reply:    acc.Reply,
		Usage:    acc.Usage,
		Stderr:   tail.String(),
		Argv:     r.ElidedArgv(req),
		Duration: time.Since(started).Round(time.Millisecond),
		Events:   *acc,
	}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	} else if waitErr != nil {
		res.ExitCode = -1
	}

	// Classify. Cancellation and timeout are checked first: they explain a
	// truncated stream, and reporting "no turn.completed" for a turn the user
	// stopped would be misleading.
	switch {
	case ctx.Err() != nil:
		res.Cancelled = true
		return res, fmt.Errorf("%w: the user stopped it after %s", ErrCancelled, res.Duration)
	case runCtx.Err() != nil && errors.Is(runCtx.Err(), context.DeadlineExceeded):
		res.TimedOut = true
		return res, fmt.Errorf("%w after %s; the process group was terminated", ErrTimeout, r.cfg.Timeout)
	}

	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			reason := acc.Reason()
			return res, fmt.Errorf("%w: exit %d (%s)", ErrExited, res.ExitCode, reason)
		}
		// Not an exit error: WaitDelay expiry, an I/O problem, a signal we did
		// not send. Report it as such rather than pretending Codex failed.
		return res, fmt.Errorf("codexcli: waiting for codex: %w", waitErr)
	}

	switch v := acc.Verdict(); v {
	case VerdictOK:
		if acc.MalformedLines > 0 {
			// The answer is complete and well formed, so the turn still
			// succeeded; a stray non-JSON line on stdout is worth a log and
			// nothing more. See README "Deliberate choices".
			r.log.Warn("codex stdout contained non-JSON lines",
				"malformed", acc.MalformedLines, "thread_id", acc.ThreadID)
		}
		return res, nil
	case VerdictTurnFailed:
		return res, fmt.Errorf("%w: %s", ErrTurnFailed, acc.Reason())
	case VerdictNoThread:
		return res, fmt.Errorf("%w: %s", ErrNoThreadID, acc.Reason())
	case VerdictIncomplete:
		return res, fmt.Errorf("%w: %s", ErrIncompleteStream, acc.Reason())
	case VerdictNoReply:
		return res, fmt.Errorf("%w: %s", ErrNoFinalReply, acc.Reason())
	default:
		return res, fmt.Errorf("codexcli: unclassified outcome %s", v)
	}
}

// watchdog escalates SIGTERM to SIGKILL for the whole process group when the
// turn's context ends.
//
// The pgid equals the child's pid because of Setpgid, and the child has not been
// reaped yet (Wait has not returned), so the pid cannot have been recycled and
// the signal cannot land on an unrelated process.
func (r *Runner) watchdog(ctx context.Context, pid int, procDone <-chan struct{}) {
	select {
	case <-procDone:
		return
	case <-ctx.Done():
	}
	if pid <= 0 {
		return
	}
	r.log.Debug("terminating codex process group", "pgid", pid)
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		r.log.Warn("SIGTERM to codex process group failed", "pgid", pid, "error", err.Error())
	}
	timer := time.NewTimer(r.cfg.KillGrace)
	defer timer.Stop()
	select {
	case <-procDone:
		return
	case <-timer.C:
	}
	r.log.Warn("codex process group ignored SIGTERM, sending SIGKILL", "pgid", pid)
	if err := syscall.Kill(-pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		r.log.Warn("SIGKILL to codex process group failed", "pgid", pid, "error", err.Error())
	}
}

// strippedEnvPrefixes are environment variables the child must not see.
//
// Codex can run shell commands inside the workspace under workspace-write. If
// the bot token were in its environment, one prompt ("print your environment")
// would be enough to exfiltrate it into a chat. The token and the allowlist are
// therefore removed from the child's environment, not merely hidden from logs.
var strippedEnvPrefixes = []string{"BOT_", "TELEGRAM_", "ALLOWED_TELEGRAM_"}

// childEnv builds the child environment: the bot's own, minus its secrets, plus
// the dedicated CODEX_HOME.
func (r *Runner) childEnv() []string {
	base := r.cfg.BaseEnv
	if base == nil {
		base = os.Environ()
	}
	out := make([]string, 0, len(base)+1)
	sawCodexHome := false
	for _, kv := range base {
		key, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if stripped(key) {
			continue
		}
		if key == "CODEX_HOME" {
			sawCodexHome = true
		}
		out = append(out, kv)
	}
	if !sawCodexHome {
		out = append(out, "CODEX_HOME="+r.cfg.CodexHome)
	} else {
		// Replace it in place: the configured home wins over whatever the
		// service environment happened to carry.
		for i, kv := range out {
			if strings.HasPrefix(kv, "CODEX_HOME=") {
				out[i] = "CODEX_HOME=" + r.cfg.CodexHome
			}
		}
	}
	return out
}

func stripped(key string) bool {
	for _, p := range strippedEnvPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// --- bounded stderr capture ------------------------------------------------

// tailBuffer is an io.Writer that keeps only its last max bytes.
//
// Codex writes progress to stderr and can be chatty; retaining the tail is what
// matters, because the reason for a failure is at the end.
type tailBuffer struct {
	mu      sync.Mutex
	max     int
	buf     []byte
	dropped int
}

func newTailBuffer(max int) *tailBuffer {
	if max <= 0 {
		max = 64 * 1024
	}
	return &tailBuffer{max: max, buf: make([]byte, 0, min(max, 4096))}
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	n := len(p)
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
		t.dropped += over
	}
	return n, nil
}

// String returns the captured tail with a marker when something was dropped.
func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.dropped == 0 {
		return string(t.buf)
	}
	return fmt.Sprintf("…[%d earlier byte(s) dropped]…%s", t.dropped, t.buf)
}

// --- capped line reader ----------------------------------------------------

// lineReader splits a stream into lines without ever holding more than max
// bytes of one line in memory. A line longer than max is drained and reported as
// oversized instead of failing the stream, because one pathological event must
// not discard a good turn.
type lineReader struct {
	br  *bufio.Reader
	max int
}

func newLineReader(r io.Reader, max int) *lineReader {
	if max <= 0 {
		max = 8 * 1024 * 1024
	}
	return &lineReader{br: bufio.NewReaderSize(r, 64*1024), max: max}
}

// Next returns the next line without its terminator. oversized reports that the
// line exceeded the cap and its content was discarded. err is io.EOF at the end
// of the stream.
func (lr *lineReader) Next() (line []byte, oversized bool, err error) {
	var buf []byte
	for {
		frag, rerr := lr.br.ReadSlice('\n')
		if len(frag) > 0 {
			if !oversized && len(buf)+len(frag) <= lr.max {
				buf = append(buf, frag...)
			} else {
				oversized = true
			}
		}
		switch {
		case rerr == nil:
			return bytes.TrimRight(buf, "\r\n"), oversized, nil
		case errors.Is(rerr, bufio.ErrBufferFull):
			// Same logical line continues; keep accumulating or draining.
			continue
		case errors.Is(rerr, io.EOF):
			if len(buf) > 0 || oversized {
				// A final line with no trailing newline. Returning it with a
				// nil error lets the next call report EOF cleanly.
				return bytes.TrimRight(buf, "\r\n"), oversized, nil
			}
			return nil, oversized, io.EOF
		default:
			return buf, oversized, rerr
		}
	}
}

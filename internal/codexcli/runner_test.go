package codexcli

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"codex-telegram-bot/internal/testkit"
)

const testThread = "0199a213-81c0-7800-8aa1-bbab2a035a53"

// env returns a minimal child environment. PATH is required: the fake Codex is a
// bash script that runs cat and mkdir.
func env(t *testing.T) []string {
	t.Helper()
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + t.TempDir(),
		"LANG=C.UTF-8",
	}
}

// setup builds a runner around a fake Codex CLI.
func setup(t *testing.T, spec testkit.CodexSpec, mutate func(*Config)) (*Runner, *testkit.FakeCodex, string) {
	t.Helper()
	fake := testkit.NewFakeCodex(t, spec)
	ws := testkit.Workspace(t)
	home := testkit.CodexHome(t)

	cfg := Config{
		Bin:            fake.Path,
		Workspace:      ws,
		CodexHome:      home,
		Sandbox:        SandboxWorkspaceWrite,
		StrictConfig:   true,
		Timeout:        20 * time.Second,
		KillGrace:      300 * time.Millisecond,
		MaxStderrBytes: 4096,
		MaxEventBytes:  1 << 20,
		BaseEnv:        env(t),
	}
	if mutate != nil {
		mutate(&cfg)
	}
	r, err := NewRunner(cfg, nil)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return r, fake, ws
}

func TestRunSucceeds(t *testing.T) {
	r, fake, ws := setup(t, testkit.CodexSpec{
		Stdout: testkit.Events(testThread, "Repo contains docs and sdk."),
	}, nil)

	res, err := r.Run(context.Background(), Request{Prompt: "Summarize this repository"})
	if err != nil {
		t.Fatalf("Run: %v\nstderr: %s", err, res.Stderr)
	}
	if res.Reply != "Repo contains docs and sdk." {
		t.Errorf("Reply = %q", res.Reply)
	}
	if res.ThreadID != testThread {
		t.Errorf("ThreadID = %q, want %q", res.ThreadID, testThread)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d", res.ExitCode)
	}
	if res.Usage.OutputTokens != 20 {
		t.Errorf("Usage = %+v", res.Usage)
	}
	if res.Duration <= 0 {
		t.Errorf("Duration = %s", res.Duration)
	}
	if res.TimedOut || res.Cancelled {
		t.Errorf("a successful turn is flagged as timed out or cancelled: %+v", res)
	}

	// The recorded command line is the one the runner claims to build.
	got := fake.Argv(t, 1)
	want := []string{
		"exec", "--json", "--strict-config", "--sandbox", "workspace-write",
		"--", "Summarize this repository",
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("codex was invoked with\n  %q\nwant\n  %q", got, want)
	}
	if cwd := fake.CWD(t, 1); cwd != ws {
		t.Errorf("codex ran in %q, want the workspace %q", cwd, ws)
	}
}

// TestResumeRunsInTheSameDirectory is the check that matters most for resume:
// `codex exec resume` has no --cd flag, so the working directory of the child
// process is the only thing that tells Codex which session to look for.
func TestResumeRunsInTheSameDirectory(t *testing.T) {
	r, fake, ws := setup(t, testkit.CodexSpec{
		Stdout: testkit.Events(testThread, "spec looks fine"),
	}, nil)

	res, err := r.Run(context.Background(), Request{ThreadID: testThread, Prompt: "Now inspect the spec file"})
	if err != nil {
		t.Fatalf("Run: %v\nstderr: %s", err, res.Stderr)
	}
	if cwd := fake.CWD(t, 1); cwd != ws {
		t.Errorf("the resumed turn ran in %q, want the same workspace %q", cwd, ws)
	}
	got := fake.Argv(t, 1)
	want := []string{
		"exec", "resume", testThread, "--json", "--strict-config",
		"-c", `sandbox_mode="workspace-write"`, "--", "Now inspect the spec file",
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("resume argv =\n  %q\nwant\n  %q", got, want)
	}
}

// TestChildEnvironmentIsScrubbed is a security test.
//
// Codex runs shell commands inside the workspace under workspace-write. If the
// bot token were in its environment, one prompt would be enough to print it into
// a chat. So the child must not see it at all.
func TestChildEnvironmentIsScrubbed(t *testing.T) {
	base := append(env(t),
		"TELEGRAM_BOT_TOKEN=123456:SUPER-SECRET-TOKEN",
		"ALLOWED_TELEGRAM_USER_IDS=111,222",
		"BOT_WORKSPACE=/should/not/leak",
		"BOT_CODEX_BIN=/should/not/leak",
		"CODEX_HOME=/an/old/value",
		"OPENAI_API_KEY=sk-kept-on-purpose-1234567890",
	)
	r, fake, _ := setup(t, testkit.CodexSpec{Stdout: testkit.Events(testThread, "ok")},
		func(c *Config) { c.BaseEnv = base })

	if _, err := r.Run(context.Background(), Request{Prompt: "hi"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	childEnv := fake.Env(t, 1)

	for _, banned := range []string{
		"TELEGRAM_BOT_TOKEN", "ALLOWED_TELEGRAM_USER_IDS",
		"BOT_WORKSPACE", "BOT_CODEX_BIN",
	} {
		if v, ok := childEnv[banned]; ok {
			t.Errorf("the child environment contains %s=%q; it must be stripped", banned, v)
		}
	}
	if got := childEnv["CODEX_HOME"]; got != r.cfg.CodexHome {
		t.Errorf("child CODEX_HOME = %q, want the configured %q", got, r.cfg.CodexHome)
	}
	// Credentials Codex itself needs must survive the scrub.
	if _, ok := childEnv["OPENAI_API_KEY"]; !ok {
		t.Error("OPENAI_API_KEY was stripped; Codex needs it to authenticate")
	}
	if _, ok := childEnv["PATH"]; !ok {
		t.Error("PATH was stripped")
	}
	// And the token must not appear anywhere in the recorded environment.
	for k, v := range childEnv {
		if strings.Contains(v, "SUPER-SECRET-TOKEN") {
			t.Errorf("the bot token appears in the child environment as %s", k)
		}
	}
}

func TestRunReportsANonzeroExit(t *testing.T) {
	r, _, _ := setup(t, testkit.CodexSpec{
		Stderr: "Error: thread/resume: no rollout found for thread id " + testThread + " (code -32600)\n",
		Exit:   1,
	}, nil)

	res, err := r.Run(context.Background(), Request{ThreadID: testThread, Prompt: "hi"})
	if err == nil {
		t.Fatal("Run succeeded although codex exited 1")
	}
	if !errors.Is(err, ErrExited) {
		t.Errorf("err = %v, want ErrExited", err)
	}
	if res.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", res.ExitCode)
	}
	if !strings.Contains(res.Stderr, "no rollout found") {
		t.Errorf("stderr was not captured: %q", res.Stderr)
	}
}

func TestRunRequiresTurnCompleted(t *testing.T) {
	r, _, _ := setup(t, testkit.CodexSpec{
		Stdout: testkit.Lines(
			`{"type":"thread.started","thread_id":"`+testThread+`"}`,
			`{"type":"turn.started"}`,
			`{"type":"item.completed","item":{"id":"i","type":"agent_message","text":"a plausible answer"}}`,
		),
		Exit: 0,
	}, nil)

	res, err := r.Run(context.Background(), Request{Prompt: "hi"})
	if err == nil {
		t.Fatal("Run accepted a stream with no turn.completed")
	}
	if !errors.Is(err, ErrIncompleteStream) {
		t.Errorf("err = %v, want ErrIncompleteStream", err)
	}
	// The thread id still came back, so the caller can persist it.
	if res.ThreadID != testThread {
		t.Errorf("ThreadID = %q, want it even on failure", res.ThreadID)
	}
}

func TestRunRequiresAFinalMessage(t *testing.T) {
	r, _, _ := setup(t, testkit.CodexSpec{
		Stdout: testkit.Lines(
			`{"type":"thread.started","thread_id":"`+testThread+`"}`,
			`{"type":"turn.completed"}`,
		),
	}, nil)
	if _, err := r.Run(context.Background(), Request{Prompt: "hi"}); !errors.Is(err, ErrNoFinalReply) {
		t.Errorf("err = %v, want ErrNoFinalReply", err)
	}
}

func TestRunReportsTurnFailed(t *testing.T) {
	r, _, _ := setup(t, testkit.CodexSpec{
		Stdout: testkit.Lines(
			`{"type":"thread.started","thread_id":"`+testThread+`"}`,
			`{"type":"turn.failed","error":{"message":"model overloaded"}}`,
		),
		Exit: 1,
	}, nil)
	_, err := r.Run(context.Background(), Request{Prompt: "hi"})
	// A nonzero exit is the outer classification; either way the turn failed and
	// the reason mentions the model.
	if err == nil {
		t.Fatal("Run accepted turn.failed")
	}
	if !errors.Is(err, ErrTurnFailed) && !errors.Is(err, ErrExited) {
		t.Errorf("err = %v, want ErrTurnFailed or ErrExited", err)
	}
	if !strings.Contains(err.Error(), "model overloaded") {
		t.Errorf("the error does not carry Codex's own reason: %v", err)
	}
}

func TestRunRequiresAThreadID(t *testing.T) {
	r, _, _ := setup(t, testkit.CodexSpec{
		Stdout: testkit.Lines(
			`{"type":"turn.started"}`,
			`{"type":"item.completed","item":{"id":"i","type":"agent_message","text":"hi"}}`,
			`{"type":"turn.completed"}`,
		),
	}, nil)
	if _, err := r.Run(context.Background(), Request{Prompt: "hi"}); !errors.Is(err, ErrNoThreadID) {
		t.Errorf("err = %v, want ErrNoThreadID", err)
	}
}

func TestRunRejectsABadRequestBeforeSpawning(t *testing.T) {
	r, fake, _ := setup(t, testkit.CodexSpec{Stdout: testkit.Events(testThread, "ok")}, nil)
	if _, err := r.Run(context.Background(), Request{Prompt: "   "}); err == nil {
		t.Fatal("Run accepted an empty prompt")
	}
	if _, err := r.Run(context.Background(), Request{ThreadID: "a-thread-name", Prompt: "hi"}); err == nil {
		t.Fatal("Run accepted a thread name instead of a UUID")
	}
	if n := len(fake.Invocations(t)); n != 0 {
		t.Errorf("codex was spawned %d time(s) for invalid requests", n)
	}
}

// TestRunTimeoutKillsTheProcessGroup checks the timeout path end to end, and
// that it reaches grandchildren: Codex spawns a shell, which spawns the agent's
// commands, and killing only the direct child would leave those running.
//
// The grandchild's pid has to be observed while the turn is still alive, so Run
// goes on a goroutine; reading the pid afterwards would only ever find a process
// the timeout had already killed.
func TestRunTimeoutKillsTheProcessGroup(t *testing.T) {
	r, fake, _ := setup(t, testkit.CodexSpec{
		Stdout:      testkit.Events(testThread, "never reached"),
		SleepBefore: 30 * time.Second,
		SpawnChild:  true,
	}, func(c *Config) {
		c.Timeout = 2 * time.Second
		c.KillGrace = 300 * time.Millisecond
	})

	type outcome struct {
		res *Result
		err error
	}
	done := make(chan outcome, 1)
	start := time.Now()
	go func() {
		res, err := r.Run(context.Background(), Request{Prompt: "a long task"})
		done <- outcome{res, err}
	}()

	pid := waitForChildPID(t, fake, 1)

	var got outcome
	select {
	case got = <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after its timeout")
	}
	if !errors.Is(got.err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", got.err)
	}
	if got.res == nil || !got.res.TimedOut {
		t.Errorf("the result is not flagged as a timeout: %+v", got.res)
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Errorf("the timeout took %s to take effect", elapsed)
	}

	waitForDeath(t, pid, 10*time.Second)
}

// TestCancellationKillsTheProcessGroup is the /stop path: the caller's context
// ends, the group is terminated, and the turn is classified as cancelled rather
// than failed or timed out.
func TestCancellationKillsTheProcessGroup(t *testing.T) {
	r, fake, _ := setup(t, testkit.CodexSpec{
		Stdout:      testkit.Events(testThread, "never reached"),
		SleepBefore: 30 * time.Second,
		SpawnChild:  true,
	}, func(c *Config) {
		c.KillGrace = 300 * time.Millisecond
	})

	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		res *Result
		err error
	}
	done := make(chan result, 1)
	go func() {
		res, err := r.Run(ctx, Request{Prompt: "a long task"})
		done <- result{res, err}
	}()

	pid := waitForChildPID(t, fake, 1)
	cancel()

	select {
	case got := <-done:
		if !errors.Is(got.err, ErrCancelled) {
			t.Errorf("err = %v, want ErrCancelled", got.err)
		}
		if got.res == nil || !got.res.Cancelled {
			t.Errorf("the result is not flagged as cancelled: %+v", got.res)
		}
		if errors.Is(got.err, ErrTimeout) {
			t.Error("a user cancellation was classified as a timeout")
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}

	waitForDeath(t, pid, 10*time.Second)
}

// TestCancellationFromAParentContext makes sure a shutdown that cancels an
// ancestor context also stops a turn, not just a direct /stop.
func TestCancellationFromAParentContext(t *testing.T) {
	r, _, _ := setup(t, testkit.CodexSpec{
		Stdout:      testkit.Events(testThread, "never reached"),
		SleepBefore: 30 * time.Second,
	}, nil)

	parent, cancelParent := context.WithCancel(context.Background())
	ctx, cancelChild := context.WithCancel(parent)
	defer cancelChild()

	done := make(chan error, 1)
	go func() { _, err := r.Run(ctx, Request{Prompt: "x"}); done <- err }()

	time.Sleep(300 * time.Millisecond) // let the child start
	cancelParent()

	select {
	case err := <-done:
		if !errors.Is(err, ErrCancelled) {
			t.Errorf("err = %v, want ErrCancelled", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Run did not return after the parent context was cancelled")
	}
}

func TestStderrIsCapturedAndBounded(t *testing.T) {
	huge := strings.Repeat("progress noise line\n", 5000) // ~100 KB
	r, _, _ := setup(t, testkit.CodexSpec{
		Stdout: testkit.Events(testThread, "ok"),
		Stderr: huge + "THE_FINAL_REASON\n",
	}, func(c *Config) { c.MaxStderrBytes = 512 })

	res, err := r.Run(context.Background(), Request{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Stderr) > 512+len("…[999999 earlier byte(s) dropped]…") {
		t.Errorf("stderr grew to %d bytes, the cap is 512", len(res.Stderr))
	}
	if !strings.Contains(res.Stderr, "THE_FINAL_REASON") {
		t.Errorf("the tail was dropped, and that is the useful part: %q", res.Stderr)
	}
	if !strings.Contains(res.Stderr, "dropped") {
		t.Errorf("the truncation is not marked: %q", res.Stderr)
	}
}

func TestOversizedEventLineIsSkippedNotFatal(t *testing.T) {
	junk := `{"type":"item.completed","item":{"text":"` + strings.Repeat("x", 5000) + `"}}`
	r, _, _ := setup(t, testkit.CodexSpec{
		Stdout: testkit.Lines(
			`{"type":"thread.started","thread_id":"`+testThread+`"}`,
			junk,
			`{"type":"item.completed","item":{"id":"i","type":"agent_message","text":"still fine"}}`,
			`{"type":"turn.completed"}`,
		),
	}, func(c *Config) { c.MaxEventBytes = 4096 })

	res, err := r.Run(context.Background(), Request{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Run: %v (one oversized line should not fail a turn)", err)
	}
	if res.Reply != "still fine" {
		t.Errorf("Reply = %q", res.Reply)
	}
	if res.Events.OversizedLines != 1 {
		t.Errorf("OversizedLines = %d, want 1", res.Events.OversizedLines)
	}
}

func TestMalformedOutputStillYieldsAGoodReply(t *testing.T) {
	r, _, _ := setup(t, testkit.CodexSpec{
		Stdout: strings.Join([]string{
			`{"type":"thread.started","thread_id":"` + testThread + `"}`,
			`codex: some stray non-JSON progress line`,
			`{"type":"item.completed","item":{"id":"i","type":"agent_message","text":"the answer"}}`,
			`{"type":"turn.completed"}`,
		}, "\n") + "\n",
	}, nil)

	res, err := r.Run(context.Background(), Request{Prompt: "hi"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Reply != "the answer" {
		t.Errorf("Reply = %q", res.Reply)
	}
	if res.Events.MalformedLines != 1 {
		t.Errorf("MalformedLines = %d, want 1", res.Events.MalformedLines)
	}
}

func TestRunRecordsTheElidedArgvNotThePrompt(t *testing.T) {
	r, _, _ := setup(t, testkit.CodexSpec{Stdout: testkit.Events(testThread, "ok")}, nil)
	res, err := r.Run(context.Background(), Request{Prompt: "a prompt with private details"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(strings.Join(res.Argv, " "), "private details") {
		t.Errorf("Result.Argv carries the prompt: %q", res.Argv)
	}
	if res.Argv[len(res.Argv)-1] != "<prompt>" {
		t.Errorf("Result.Argv = %q, want a placeholder last", res.Argv)
	}
}

// TestRunGivesTheChildNoStdin covers a real hazard: Codex reads stdin even when
// a prompt argument is supplied, and appends any piped content as a <stdin>
// block. So the child must get /dev/null — otherwise it would either block
// forever or absorb whatever the bot's own stdin happened to be.
//
// The fake Codex runs `cat > stdin.N` before producing output. If stdin were an
// open pipe this test would hang instead of passing, which is the point.
func TestRunGivesTheChildNoStdin(t *testing.T) {
	r, fake, _ := setup(t, testkit.CodexSpec{
		Stdout: testkit.Events(testThread, "ok"),
	}, nil)
	if _, err := r.Run(context.Background(), Request{Prompt: "hi"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := fake.Stdin(t, 1); got != "" {
		t.Errorf("the child read %d byte(s) from stdin: %q", len(got), got)
	}
}

func TestProbeVersionAndLoginStatus(t *testing.T) {
	r, _, _ := setup(t, testkit.CodexSpec{Stdout: "codex-cli 9.9.9-test\n"}, nil)
	// The fake prints the same thing for every invocation, which is enough to
	// exercise the probe plumbing.
	v, err := r.Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if !strings.Contains(v, "codex-cli") {
		t.Errorf("Version = %q", v)
	}

	loggedIn, detail, err := r.LoginStatus(context.Background())
	if err != nil {
		t.Fatalf("LoginStatus: %v", err)
	}
	// "codex-cli 9.9.9-test" contains neither phrase, so the honest answer is
	// "not recognised" and loggedIn must be false rather than guessed.
	if loggedIn {
		t.Error("LoginStatus reported a login from unrecognised output")
	}
	if detail == "" {
		t.Error("LoginStatus returned no detail")
	}
}

func TestLoginStatusRecognisesBothAnswers(t *testing.T) {
	cases := []struct {
		out  string
		want bool
	}{
		{"Logged in using ChatGPT\n", true},
		{"Logged in using API key\n", true},
		{"Not logged in\n", false},
	}
	for _, c := range cases {
		r, _, _ := setup(t, testkit.CodexSpec{Stdout: c.out}, nil)
		got, detail, err := r.LoginStatus(context.Background())
		if err != nil {
			t.Fatalf("LoginStatus: %v", err)
		}
		if got != c.want {
			t.Errorf("LoginStatus(%q) = %v, want %v (detail %q)", c.out, got, c.want, detail)
		}
		if strings.Contains(detail, "\n") {
			t.Errorf("detail is not a single line: %q", detail)
		}
	}
}

func TestProbeReportsAFailingBinary(t *testing.T) {
	r, _, _ := setup(t, testkit.CodexSpec{Stderr: "boom\n", Exit: 3}, nil)
	if _, err := r.Version(context.Background()); err == nil {
		t.Fatal("Version succeeded although the binary exited 3")
	} else if !strings.Contains(err.Error(), "boom") {
		t.Errorf("the error does not carry the binary's output: %v", err)
	}
}

// --- helpers ---------------------------------------------------------------

// waitForChildPID polls until the fake Codex has recorded the pid of the
// background process it spawned.
func waitForChildPID(t *testing.T, fake *testkit.FakeCodex, n int) int {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if pid := fake.ChildPID(t, n); pid != 0 {
			if !testkit.Alive(pid) {
				t.Fatalf("the grandchild %d died before the test could observe it", pid)
			}
			return pid
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the fake codex never recorded a background child pid")
	return 0
}

// waitForDeath fails unless pid is gone within the deadline.
func waitForDeath(t *testing.T, pid int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if !testkit.Alive(pid) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Errorf("pid %d was still alive %s after the process group was signalled", pid, within)
}

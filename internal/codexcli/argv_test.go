package codexcli

import (
	"slices"
	"strings"
	"testing"
)

func testRunner(t *testing.T, mutate func(*Config)) *Runner {
	t.Helper()
	cfg := Config{
		Bin:          "/usr/bin/codex",
		Workspace:    "/work/repo",
		CodexHome:    "/home/u/.local/share/bot/codex",
		Sandbox:      SandboxWorkspaceWrite,
		StrictConfig: true,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	r, err := NewRunner(cfg, nil)
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	return r
}

// TestArgvNewTurn pins the exact command line for a first turn.
//
// This is the list README.md documents, so a change here must be a deliberate
// one: it is verified against `codex exec --help` on the installed CLI.
func TestArgvNewTurn(t *testing.T) {
	r := testRunner(t, nil)
	got, err := r.Argv(Request{Prompt: "Summarize this repository"})
	if err != nil {
		t.Fatalf("Argv: %v", err)
	}
	want := []string{
		"exec",
		"--json",
		"--strict-config",
		"--sandbox", "workspace-write",
		"--", "Summarize this repository",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("argv =\n  %q\nwant\n  %q", got, want)
	}
}

// TestArgvResumeTurn pins the exact command line for a later turn.
//
// The stored thread id is passed positionally and verbatim. --last and --all are
// never used: either could select a different session than the one this Telegram
// user owns.
func TestArgvResumeTurn(t *testing.T) {
	const thread = "0199a213-81c0-7800-8aa1-bbab2a035a53"
	r := testRunner(t, nil)
	got, err := r.Argv(Request{ThreadID: thread, Prompt: "Now inspect the spec file"})
	if err != nil {
		t.Fatalf("Argv: %v", err)
	}
	want := []string{
		"exec", "resume", thread,
		"--json",
		"--strict-config",
		// `codex exec resume` has no --sandbox flag in codex-cli 0.156.1, so the
		// policy travels as a config override. The value is quoted so it parses
		// as TOML instead of falling back to the raw-string path.
		"-c", `sandbox_mode="workspace-write"`,
		"--", "Now inspect the spec file",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("argv =\n  %q\nwant\n  %q", got, want)
	}
	for _, banned := range []string{"--last", "--all", "--ephemeral", "-s", "--sandbox"} {
		if slices.Contains(got, banned) {
			t.Errorf("resume argv must not contain %q, got %q", banned, got)
		}
	}
}

// TestArgvNeverUsesTheDangerousFlags covers both paths at once.
func TestArgvNeverUsesTheDangerousFlags(t *testing.T) {
	r := testRunner(t, nil)
	for _, req := range []Request{
		{Prompt: "x"},
		{ThreadID: "0199a213-81c0-7800-8aa1-bbab2a035a53", Prompt: "x"},
	} {
		got, err := r.Argv(req)
		if err != nil {
			t.Fatalf("Argv: %v", err)
		}
		joined := strings.Join(got, " ")
		for _, banned := range []string{
			"--dangerously-bypass-approvals-and-sandbox",
			"--dangerously-bypass-hook-trust",
			"danger-full-access",
			"--ephemeral",
			"--skip-git-repo-check",
			"--approve-for-me",
			"--worktree",
		} {
			if strings.Contains(joined, banned) {
				t.Errorf("argv contains %q: %q", banned, joined)
			}
		}
	}
}

// TestArgvPromptIsAfterTheTerminator is the injection test: a prompt that looks
// like a flag, a shell fragment or both must still arrive as one literal
// argument, after `--`, and never be interpreted.
func TestArgvPromptIsAfterTheTerminator(t *testing.T) {
	hostile := []string{
		"-sandbox danger-full-access",
		"--dangerously-bypass-approvals-and-sandbox",
		"-rm -rf /",
		"; rm -rf ~ #",
		"$(whoami)",
		"`id`",
		"--",
		"-",
		"\nnewline\nand \"quotes\" and 'single'",
		strings.Repeat("long ", 2000),
	}
	r := testRunner(t, nil)
	for _, prompt := range hostile {
		got, err := r.Argv(Request{Prompt: prompt})
		if err != nil {
			t.Fatalf("Argv(%q): %v", prompt, err)
		}
		// The prompt is exactly one element, and it is the last one.
		if got[len(got)-1] != prompt {
			t.Errorf("the prompt is not the final argument verbatim: %q", got[len(got)-1])
		}
		// The element before it is the terminator, so nothing in the prompt can
		// be read as an option.
		if got[len(got)-2] != "--" {
			t.Errorf("the prompt is not preceded by `--`: %q", got)
		}
		// And no extra element appeared: one prompt in, one argument out.
		want := len(testRunner(t, nil).mustArgv(t, Request{Prompt: "x"}))
		if len(got) != want {
			t.Errorf("argv has %d elements for a hostile prompt, want %d: %q", len(got), want, got)
		}
	}
}

func (r *Runner) mustArgv(t *testing.T, req Request) []string {
	t.Helper()
	got, err := r.Argv(req)
	if err != nil {
		t.Fatalf("Argv: %v", err)
	}
	return got
}

// TestArgvNoShellIsInvolved asserts the binary is executed directly: the runner
// never builds a command string, so there is no shell to interpret anything.
func TestArgvNoShellIsInvolved(t *testing.T) {
	r := testRunner(t, nil)
	got := r.mustArgv(t, Request{Prompt: "anything"})
	for _, a := range got {
		if a == "-c" && strings.Contains(a, "sh ") {
			t.Fatalf("argv looks like a shell invocation: %q", got)
		}
	}
	if got[0] != "exec" {
		t.Errorf("argv[0] = %q, want the codex subcommand directly (no `sh -c`)", got[0])
	}
}

func TestArgvWithModel(t *testing.T) {
	r := testRunner(t, func(c *Config) { c.Model = "gpt-5-codex" })
	got := r.mustArgv(t, Request{Prompt: "x"})
	i := slices.Index(got, "-m")
	if i < 0 || got[i+1] != "gpt-5-codex" {
		t.Errorf("argv is missing `-m gpt-5-codex`: %q", got)
	}
}

func TestArgvWithoutStrictConfig(t *testing.T) {
	r := testRunner(t, func(c *Config) { c.StrictConfig = false })
	for _, req := range []Request{{Prompt: "x"}, {ThreadID: "0199a213-81c0-7800-8aa1-bbab2a035a53", Prompt: "x"}} {
		if slices.Contains(r.mustArgv(t, req), "--strict-config") {
			t.Errorf("--strict-config was passed although it is disabled: %q", r.mustArgv(t, req))
		}
	}
}

func TestArgvReadOnlySandbox(t *testing.T) {
	r := testRunner(t, func(c *Config) { c.Sandbox = SandboxReadOnly })
	got := r.mustArgv(t, Request{Prompt: "x"})
	i := slices.Index(got, "--sandbox")
	if i < 0 || got[i+1] != "read-only" {
		t.Errorf("argv = %q, want --sandbox read-only", got)
	}
	resume := r.mustArgv(t, Request{ThreadID: "0199a213-81c0-7800-8aa1-bbab2a035a53", Prompt: "x"})
	if !slices.Contains(resume, `sandbox_mode="read-only"`) {
		t.Errorf("resume argv = %q, want the read-only config override", resume)
	}
}

func TestNewRunnerRejectsDangerFullAccess(t *testing.T) {
	_, err := NewRunner(Config{
		Bin: "/usr/bin/codex", Workspace: "/w", CodexHome: "/h",
		Sandbox: "danger-full-access",
	}, nil)
	if err == nil {
		t.Fatal("NewRunner accepted danger-full-access")
	}
	if !strings.Contains(err.Error(), "refused on purpose") {
		t.Errorf("the error should say the refusal is deliberate: %v", err)
	}
}

func TestNewRunnerRequiresItsPaths(t *testing.T) {
	for name, cfg := range map[string]Config{
		"no bin":       {Workspace: "/w", CodexHome: "/h"},
		"no workspace": {Bin: "/bin/true", CodexHome: "/h"},
		"no home":      {Bin: "/bin/true", Workspace: "/w"},
	} {
		if _, err := NewRunner(cfg, nil); err == nil {
			t.Errorf("NewRunner accepted a config with %s", name)
		}
	}
}

func TestArgvRejectsABadThreadID(t *testing.T) {
	r := testRunner(t, nil)
	bad := []string{
		"", // handled as "new turn", so not here
		"not-a-uuid",
		"my-session-name",                       // a thread *name*: Codex accepts one, and that is the ambiguity to avoid
		"0199a213-81c0-7800-8aa1-bbab2a035a5",   // too short
		"0199a213-81c0-7800-8aa1-bbab2a035a533", // too long
		"0199a213-81c0-7800-8aa1-zzzz2a035a53",  // non-hex
		"--last",
		"s7k3qm2", // a bot session id must never reach the Codex command line
	}
	for _, id := range bad[1:] {
		if _, err := r.Argv(Request{ThreadID: id, Prompt: "x"}); err == nil {
			t.Errorf("Argv accepted the thread id %q", id)
		}
	}
	// An empty thread id is the "start a new thread" case, not an error.
	if _, err := r.Argv(Request{Prompt: "x"}); err != nil {
		t.Errorf("Argv rejected an empty thread id: %v", err)
	}
}

func TestArgvRejectsAnEmptyPrompt(t *testing.T) {
	r := testRunner(t, nil)
	for _, p := range []string{"", "   ", "\n\t "} {
		if _, err := r.Argv(Request{Prompt: p}); err == nil {
			t.Errorf("Argv accepted the empty prompt %q", p)
		}
	}
}

func TestElidedArgvHidesThePrompt(t *testing.T) {
	r := testRunner(t, nil)
	got := r.ElidedArgv(Request{ThreadID: "0199a213-81c0-7800-8aa1-bbab2a035a53", Prompt: "delete all my secrets"})
	joined := strings.Join(got, " ")
	if strings.Contains(joined, "secrets") {
		t.Errorf("ElidedArgv leaked the prompt: %q", joined)
	}
	if got[len(got)-1] != "<prompt>" {
		t.Errorf("ElidedArgv did not substitute a placeholder: %q", got)
	}
	if !slices.Contains(got, "0199a213-81c0-7800-8aa1-bbab2a035a53") {
		t.Errorf("ElidedArgv dropped the thread id, which is the part worth keeping: %q", got)
	}
	// An invalid request still has to produce something storable.
	if got := r.ElidedArgv(Request{Prompt: ""}); len(got) == 0 {
		t.Error("ElidedArgv returned nothing for an invalid request")
	}
}

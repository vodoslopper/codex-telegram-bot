package bot

import (
	"strings"
	"testing"

	"codex-telegram-bot/internal/testkit"
)

func TestModelSettingsChooseTurnModelAndSurviveRestart(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("first", "next")})
	expectModel := func(n int, want string) {
		t.Helper()
		argv := h.fake.Argv(t, n)
		for i := range argv {
			if argv[i] == "-m" && i+1 < len(argv) {
				if argv[i+1] != want {
					t.Errorf("turn %d model = %q, want %q", n, argv[i+1], want)
				}
				return
			}
		}
		t.Errorf("turn %d has no model flag: %q", n, argv)
	}
	h.text(msg(1, aliceChat, aliceID, "hello"))
	expectModel(1, "gpt-6-sol")
	if got := h.text(msg(2, aliceChat, aliceID, "/model default luna")); !strings.Contains(got, "gpt-6-luna") {
		t.Fatal(got)
	}
	h.text(msg(3, aliceChat, aliceID, "again"))
	expectModel(2, "gpt-6-luna") // resumed thread
	h.text(msg(4, aliceChat, aliceID, "/model sol"))
	h.text(msg(5, aliceChat, aliceID, "third"))
	expectModel(3, "gpt-6-sol")
	h.text(msg(6, aliceChat, aliceID, "/model reset"))
	h.text(msg(7, aliceChat, aliceID, "fourth"))
	expectModel(4, "gpt-6-luna")
	h.Close()
	restarted := newHarness(t, harnessOpts{ws: h.ws, state: h.state, home: h.home, fake: h.fake})
	if got := restarted.text(msg(8, aliceChat, aliceID, "/model")); !strings.Contains(got, "gpt-6-luna") {
		t.Fatal(got)
	}
	restarted.text(msg(9, aliceChat, aliceID, "after restart"))
	expectModel(5, "gpt-6-luna")
}

func TestModelSettingsTopicIsolationAndInvalidChoice(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("ok", "ok")})
	h.text(msg(1, aliceChat, aliceID, "/model default luna"))
	h.text(testkit.TopicUpdate(2, aliceChat, 10, aliceID, "/model sol"))
	if got := h.text(testkit.TopicUpdate(3, aliceChat, 10, aliceID, "/model")); !strings.Contains(got, "gpt-6-sol") {
		t.Fatal(got)
	}
	if got := h.text(testkit.TopicUpdate(4, aliceChat, 11, aliceID, "/model")); !strings.Contains(got, "gpt-6-luna") {
		t.Fatal(got)
	}
	if got := h.text(msg(5, bobChat, bobID, "/model")); !strings.Contains(got, "gpt-6-sol") {
		t.Fatal(got)
	}
	if got := h.text(msg(6, aliceChat, aliceID, "/model astra")); !strings.Contains(got, "Usage:") {
		t.Fatal(got)
	}
	if got := h.text(msg(7, aliceChat, aliceID, "/model")); !strings.Contains(got, "gpt-6-luna") {
		t.Fatal(got)
	}
}

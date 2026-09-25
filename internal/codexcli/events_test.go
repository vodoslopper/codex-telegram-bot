package codexcli

import (
	"strings"
	"testing"

	"codex-telegram-bot/internal/testkit"
)

// documentedStream is the JSONL sample from the Codex non-interactive-mode
// documentation, which matches what codex-cli 0.156.1 emits.
const documentedStream = `{"type":"thread.started","thread_id":"0199a213-81c0-7800-8aa1-bbab2a035a53"}
{"type":"turn.started"}
{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"bash -lc ls","status":"in_progress"}}
{"type":"item.completed","item":{"id":"item_3","type":"agent_message","text":"Repo contains docs, sdk, and examples directories."}}
{"type":"turn.completed","usage":{"input_tokens":24763,"cached_input_tokens":24448,"output_tokens":122,"reasoning_output_tokens":0}}
`

func feed(t *testing.T, a *Accumulator, stream string) {
	t.Helper()
	for _, line := range strings.Split(stream, "\n") {
		a.Handle([]byte(line))
	}
}

func TestAccumulateDocumentedStream(t *testing.T) {
	var a Accumulator
	feed(t, &a, documentedStream)

	if a.ThreadID != "0199a213-81c0-7800-8aa1-bbab2a035a53" {
		t.Errorf("ThreadID = %q", a.ThreadID)
	}
	if !a.TurnStarted {
		t.Error("turn.started was not recorded")
	}
	if !a.Completed {
		t.Error("turn.completed was not recorded")
	}
	if a.Failed {
		t.Error("the turn is marked failed")
	}
	want := "Repo contains docs, sdk, and examples directories."
	if a.Reply != want {
		t.Errorf("Reply = %q, want %q", a.Reply, want)
	}
	if a.AgentMessages != 1 {
		t.Errorf("AgentMessages = %d, want 1", a.AgentMessages)
	}
	if a.Usage.InputTokens != 24763 || a.Usage.OutputTokens != 122 || a.Usage.CachedInputTokens != 24448 {
		t.Errorf("Usage = %+v", a.Usage)
	}
	if a.MalformedLines != 0 || a.UnknownEvents != 0 || a.OversizedLines != 0 {
		t.Errorf("a clean stream was not parsed cleanly: %+v", a)
	}
	if v := a.Verdict(); v != VerdictOK {
		t.Errorf("Verdict = %s, want ok", v)
	}
}

// TestErrorEventsAreNotFatal records behaviour observed from codex-cli 0.156.1:
// an "error" event carries transient stream problems such as
//
//	{"type":"error","message":"Reconnecting... 2/5 (unexpected status 401 ...)"}
//
// and Codex may still recover and complete the turn. Treating every error event
// as a failure would report a successful turn as broken.
func TestErrorEventsAreNotFatal(t *testing.T) {
	stream := `{"type":"thread.started","thread_id":"0199a213-81c0-7800-8aa1-bbab2a035a53"}
{"type":"turn.started"}
{"type":"error","message":"Reconnecting... 2/5 (unexpected status 500)"}
{"type":"error","message":"Reconnecting... 3/5 (unexpected status 500)"}
{"type":"item.completed","item":{"id":"i","type":"agent_message","text":"recovered fine"}}
{"type":"turn.completed","usage":{"input_tokens":1}}
`
	var a Accumulator
	feed(t, &a, stream)
	if v := a.Verdict(); v != VerdictOK {
		t.Fatalf("Verdict = %s, want ok after a transient error event", v)
	}
	if a.Reply != "recovered fine" {
		t.Errorf("Reply = %q", a.Reply)
	}
	if len(a.ErrorNotes) == 0 {
		t.Error("the transient errors were not kept for diagnostics")
	}
}

func TestAccumulateTurnFailed(t *testing.T) {
	var a Accumulator
	feed(t, &a, testkit.Lines(
		`{"type":"thread.started","thread_id":"0199a213-81c0-7800-8aa1-bbab2a035a53"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"id":"i","type":"agent_message","text":"a partial answer"}}`,
		`{"type":"turn.failed","error":{"message":"model overloaded"}}`,
	))
	if v := a.Verdict(); v != VerdictTurnFailed {
		t.Fatalf("Verdict = %s, want turn.failed", v)
	}
	if !a.Failed {
		t.Error("Failed is false")
	}
	// turn.failed outranks a partial agent message: a half answer must not be
	// presented as the result.
	if !strings.Contains(a.Reason(), "model overloaded") {
		t.Errorf("Reason = %q does not carry the failure message", a.Reason())
	}
}

func TestVerdicts(t *testing.T) {
	cases := []struct {
		name   string
		stream string
		want   Verdict
	}{
		{
			"no thread id",
			testkit.Lines(`{"type":"turn.started"}`, `{"type":"turn.completed"}`),
			VerdictNoThread,
		},
		{
			"no turn.completed",
			testkit.Lines(
				`{"type":"thread.started","thread_id":"0199a213-81c0-7800-8aa1-bbab2a035a53"}`,
				`{"type":"turn.started"}`,
				`{"type":"item.completed","item":{"id":"i","type":"agent_message","text":"hi"}}`,
			),
			VerdictIncomplete,
		},
		{
			"no agent message",
			testkit.Lines(
				`{"type":"thread.started","thread_id":"0199a213-81c0-7800-8aa1-bbab2a035a53"}`,
				`{"type":"turn.started"}`,
				`{"type":"item.completed","item":{"id":"i","type":"command_execution","output":"ls"}}`,
				`{"type":"turn.completed"}`,
			),
			VerdictNoReply,
		},
		{
			"empty agent message",
			testkit.Lines(
				`{"type":"thread.started","thread_id":"0199a213-81c0-7800-8aa1-bbab2a035a53"}`,
				`{"type":"item.completed","item":{"id":"i","type":"agent_message","text":"   "}}`,
				`{"type":"turn.completed"}`,
			),
			VerdictNoReply,
		},
		{
			"turn.failed beats everything",
			testkit.Lines(
				`{"type":"turn.failed","error":{"message":"boom"}}`,
			),
			VerdictTurnFailed,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var a Accumulator
			feed(t, &a, c.stream)
			if got := a.Verdict(); got != c.want {
				t.Errorf("Verdict = %s, want %s", got, c.want)
			}
		})
	}
}

func TestLastAgentMessageWins(t *testing.T) {
	var a Accumulator
	feed(t, &a, testkit.Lines(
		`{"type":"thread.started","thread_id":"0199a213-81c0-7800-8aa1-bbab2a035a53"}`,
		`{"type":"item.completed","item":{"id":"1","type":"agent_message","text":"first"}}`,
		`{"type":"item.completed","item":{"id":"2","type":"agent_message","text":"second"}}`,
		`{"type":"turn.completed"}`,
	))
	if a.Reply != "second" {
		t.Errorf("Reply = %q, want the last agent message", a.Reply)
	}
	if a.AgentMessages != 2 {
		t.Errorf("AgentMessages = %d, want 2", a.AgentMessages)
	}
}

func TestReasoningIsNeverCaptured(t *testing.T) {
	var a Accumulator
	feed(t, &a, testkit.Lines(
		`{"type":"thread.started","thread_id":"0199a213-81c0-7800-8aa1-bbab2a035a53"}`,
		`{"type":"item.completed","item":{"id":"1","type":"reasoning","text":"SECRET HIDDEN REASONING"}}`,
		`{"type":"item.completed","item":{"id":"2","type":"agent_message","text":"the answer"}}`,
		`{"type":"turn.completed"}`,
	))
	if a.Reply != "the answer" {
		t.Errorf("Reply = %q", a.Reply)
	}
	if strings.Contains(a.Reason(), "SECRET HIDDEN REASONING") {
		t.Errorf("hidden reasoning leaked into the diagnostics: %q", a.Reason())
	}
}

func TestUnknownEventsAreIgnoredNotForwarded(t *testing.T) {
	var a Accumulator
	feed(t, &a, testkit.Lines(
		`{"type":"thread.started","thread_id":"0199a213-81c0-7800-8aa1-bbab2a035a53"}`,
		`{"type":"turn.started"}`,
		`{"type":"brand.new.event.from.a.future.release","payload":{"secret":"x"}}`,
		`{"type":"another.one"}`,
		`{"type":"item.completed","item":{"id":"i","type":"agent_message","text":"ok"}}`,
		`{"type":"turn.completed"}`,
	))
	if a.UnknownEvents != 2 {
		t.Errorf("UnknownEvents = %d, want 2", a.UnknownEvents)
	}
	if v := a.Verdict(); v != VerdictOK {
		t.Errorf("Verdict = %s; an unknown event kind must not break a working turn", v)
	}
	if strings.Contains(a.Reply, "future") {
		t.Errorf("an unknown event was forwarded into the reply: %q", a.Reply)
	}
	if len(a.UnknownExamples) == 0 {
		t.Error("no sample of the unknown kinds was kept for the log")
	}
}

func TestMalformedLinesAreCountedNotFatal(t *testing.T) {
	var a Accumulator
	feed(t, &a, testkit.Lines(
		`{"type":"thread.started","thread_id":"0199a213-81c0-7800-8aa1-bbab2a035a53"}`,
		`this is not json at all`,
		`{"type":`,
		``,
		`{"notype":"x"}`,
		`{"type":"item.completed","item":{"id":"i","type":"agent_message","text":"still fine"}}`,
		`{"type":"turn.completed"}`,
	))
	if a.MalformedLines < 3 {
		t.Errorf("MalformedLines = %d, want at least 3", a.MalformedLines)
	}
	if v := a.Verdict(); v != VerdictOK {
		t.Errorf("Verdict = %s; a stray non-JSON line must not discard a complete answer", v)
	}
	if !strings.Contains(a.Reason(), "malformed") {
		t.Errorf("Reason = %q does not mention the malformed lines", a.Reason())
	}
}

func TestThreadIDIsRecordedOnceAndFirst(t *testing.T) {
	var a Accumulator
	feed(t, &a, testkit.Lines(
		`{"type":"thread.started","thread_id":"0199a213-81c0-7800-8aa1-bbab2a035a53"}`,
		`{"type":"thread.started","thread_id":"99999999-9999-9999-9999-999999999999"}`,
	))
	if a.ThreadID != "0199a213-81c0-7800-8aa1-bbab2a035a53" {
		t.Errorf("ThreadID = %q, want the first one reported", a.ThreadID)
	}
}

func TestErrorMessageShapes(t *testing.T) {
	cases := []struct{ event, want string }{
		{`{"type":"error","message":"plain message"}`, "plain message"},
		{`{"type":"turn.failed","error":{"code":"E42"}}`, "code E42"},
		{`{"type":"turn.failed","error":{"type":"stream"}}`, "stream"},
		{`{"type":"error"}`, ""},
	}
	for _, c := range cases {
		var a Accumulator
		feed(t, &a, testkit.Lines(c.event))
		if c.want == "" {
			if len(a.ErrorNotes) != 0 {
				t.Errorf("%s produced a note %q, want none", c.event, a.ErrorNotes)
			}
			continue
		}
		if len(a.ErrorNotes) == 0 || !strings.Contains(a.ErrorNotes[len(a.ErrorNotes)-1], c.want) {
			t.Errorf("%s produced notes %q, want one containing %q", c.event, a.ErrorNotes, c.want)
		}
	}
}

func TestErrorNotesAreBounded(t *testing.T) {
	var a Accumulator
	for i := 0; i < 100; i++ {
		a.Handle([]byte(`{"type":"error","message":"noise"}`))
	}
	if len(a.ErrorNotes) > maxErrorNotes {
		t.Errorf("ErrorNotes grew to %d, want at most %d", len(a.ErrorNotes), maxErrorNotes)
	}
}

func TestUnknownEventExamplesAreBounded(t *testing.T) {
	var a Accumulator
	for i := 0; i < 100; i++ {
		a.Handle([]byte(`{"type":"mystery.event"}`))
	}
	if a.UnknownEvents != 100 {
		t.Errorf("UnknownEvents = %d, want 100", a.UnknownEvents)
	}
	if len(a.UnknownExamples) > maxUnknownExamples {
		t.Errorf("UnknownExamples grew to %d, want at most %d", len(a.UnknownExamples), maxUnknownExamples)
	}
}

func TestVerdictStrings(t *testing.T) {
	for v, want := range map[Verdict]string{
		VerdictOK:         "ok",
		VerdictTurnFailed: "turn.failed",
		VerdictNoThread:   "no thread.started",
		VerdictIncomplete: "no turn.completed",
		VerdictNoReply:    "no final agent message",
		Verdict(99):       "unknown",
	} {
		if got := v.String(); got != want {
			t.Errorf("Verdict(%d).String() = %q, want %q", v, got, want)
		}
	}
}

package bot

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"codex-telegram-bot/internal/sessid"
	"codex-telegram-bot/internal/store"
	"codex-telegram-bot/internal/telegram"
	"codex-telegram-bot/internal/testkit"
	"codex-telegram-bot/internal/textsplit"
)

// threadOne is the Codex thread id the fake CLI reports for a new turn.
var threadOne = testkit.UUID(1)

// slowSpec is a fake Codex that reports its thread, then goes quiet for a long
// time and never finishes the turn. That is the shape a cancellation has to deal
// with: thread.started has already arrived, so the session must keep the id even
// though no answer ever comes.
func slowSpec() testkit.CodexSpec {
	return testkit.CodexSpec{
		Stdout: testkit.Lines(
			`{"type":"thread.started","thread_id":"`+threadOne+`"}`,
			`{"type":"turn.started"}`,
		),
		AppendStdout: testkit.Lines(
			`{"type":"item.completed","item":{"id":"i","type":"agent_message","text":"never delivered"}}`,
			`{"type":"turn.completed"}`,
		),
		SleepBefore: 20 * time.Second,
	}
}

// successSpec is a fake Codex that answers a new turn and, differently, a
// resumed one — so a test can tell which path ran without inspecting argv.
func successSpec(newReply, resumeReply string) testkit.CodexSpec {
	return testkit.CodexSpec{
		Stdout:       testkit.Events(threadOne, newReply),
		ResumeStdout: testkit.Events(threadOne, resumeReply),
	}
}

// newSession creates and selects a session through the bot's own command path,
// and returns its id.
//
// The id is read back out of the bot's reply rather than out of the store, which
// also checks that the message the user sees names a session they can use with
// /use.
func newSession(t *testing.T, h *harness, updateID int64, chatID, userID int64, name string) string {
	t.Helper()
	before := len(h.sessions(t, userID, true))
	reply := h.text(msg(updateID, chatID, userID, "/new "+name))
	if !strings.Contains(reply, "Created session") {
		t.Fatalf("/new replied %q", reply)
	}
	id := sessionIDIn(reply)
	if id == "" {
		t.Fatalf("/new did not name a session id: %q", reply)
	}
	after := h.sessions(t, userID, true)
	if len(after) != before+1 {
		t.Fatalf("after /new there are %d session(s), want %d", len(after), before+1)
	}
	if sel := selected(t, h, chatID, 0, userID); sel != id {
		t.Fatalf("/new created %s but selected %s", id, sel)
	}
	return id
}

// sessionIDIn finds a bot session id in a message.
func sessionIDIn(text string) string {
	for _, field := range strings.FieldsFunc(text, func(r rune) bool {
		return r == ' ' || r == '\n' || r == '.' || r == ',' || r == '(' || r == ')'
	}) {
		if sessid.Valid(field) {
			return field
		}
	}
	return ""
}

// --- the acceptance scenario ----------------------------------------------

// TestAcceptanceScenario walks the four steps the README promises, in order.
func TestAcceptanceScenario(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec(
		"docs, sdk and examples.", "the spec requires golang and systemd.")})

	// 1. /new packaging, then a prompt: a new Codex turn whose thread id is saved.
	h.text(msg(1, aliceChat, aliceID, "/new packaging"))
	sessions := h.sessions(t, aliceID, false)
	if len(sessions) != 1 {
		t.Fatalf("after /new there are %d session(s)", len(sessions))
	}
	id := sessions[0].ID
	if sessions[0].Name != "packaging" {
		t.Errorf("session name = %q, want %q", sessions[0].Name, "packaging")
	}
	if sessions[0].HasThread() {
		t.Error("a brand new session already has a Codex thread")
	}

	reply := h.text(msg(2, aliceChat, aliceID, "Summarize this repository"))
	if reply != "docs, sdk and examples." {
		t.Errorf("the first reply = %q", reply)
	}
	wantArgv := []string{"exec", "--json", "--strict-config", "--sandbox", "workspace-write",
		"-m", "gpt-6-sol", "--", "Summarize this repository" + fileHandoffInstruction}
	if got := h.fake.Argv(t, 1); strings.Join(got, "\x00") != strings.Join(wantArgv, "\x00") {
		t.Errorf("the first turn ran\n  %q\nwant\n  %q", got, wantArgv)
	}
	sess, err := h.st.GetSession(context.Background(), id, aliceID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.CodexThreadID != threadOne {
		t.Errorf("stored thread id = %q, want the UUID Codex reported (%q)", sess.CodexThreadID, threadOne)
	}
	turn := h.lastTurn(t, id)
	if turn.Status != store.TurnCompleted || !turn.Delivered {
		t.Errorf("turn = %+v, want completed and delivered", turn)
	}

	// 2. A second message resumes that exact UUID.
	reply = h.text(msg(3, aliceChat, aliceID, "Now inspect the spec file"))
	if reply != "the spec requires golang and systemd." {
		t.Errorf("the resumed reply = %q", reply)
	}
	wantArgv = []string{"exec", "resume", threadOne, "--json", "--strict-config",
		"-c", `sandbox_mode="workspace-write"`, "-m", "gpt-6-sol", "--", "Now inspect the spec file" + fileHandoffInstruction}
	if got := h.fake.Argv(t, 2); strings.Join(got, "\x00") != strings.Join(wantArgv, "\x00") {
		t.Errorf("the resumed turn ran\n  %q\nwant\n  %q", got, wantArgv)
	}
	if n := len(h.fake.Invocations(t)); n != 2 {
		t.Errorf("codex was invoked %d times, want 2", n)
	}

	// 3. A second session, switching back with /use, then a restart.
	h.text(msg(4, aliceChat, aliceID, "/new other"))
	second := h.sessions(t, aliceID, false)
	if len(second) != 2 {
		t.Fatalf("after a second /new there are %d session(s)", len(second))
	}
	var otherID string
	for _, s := range second {
		if s.ID != id {
			otherID = s.ID
		}
	}
	if cur := selected(t, h, aliceChat, 0, aliceID); cur != otherID {
		t.Errorf("the newest session is not selected: %q, want %q", cur, otherID)
	}
	if got := h.text(msg(5, aliceChat, aliceID, "/use "+id)); !strings.Contains(got, "Selected "+id) {
		t.Errorf("/use replied %q", got)
	}
	if cur := selected(t, h, aliceChat, 0, aliceID); cur != id {
		t.Errorf("after /use the selection is %q, want %q", cur, id)
	}

	// The restart: same workspace, same state directory, same CODEX_HOME, a new
	// process.
	h.Close()
	// The restarted harness reuses the first fake Codex binary, so the payloads
	// are the first harness's: a resumed turn answers with the resume reply.
	restarted := newHarness(t, harnessOpts{
		ws: h.ws, state: h.state, home: h.home, fake: h.fake,
	})
	if got := restarted.text(msg(6, aliceChat, aliceID, "/session")); !strings.Contains(got, threadOne) {
		t.Errorf("after a restart /session does not show the stored thread:\n%s", got)
	}
	if reply = restarted.text(msg(7, aliceChat, aliceID, "What did we discuss?")); reply != "the spec requires golang and systemd." {
		t.Errorf("after a restart the reply = %q; the resume path was not taken", reply)
	}
	wantArgv = []string{"exec", "resume", threadOne, "--json", "--strict-config",
		"-c", `sandbox_mode="workspace-write"`, "-m", "gpt-6-sol", "--", "What did we discuss?" + fileHandoffInstruction}
	if got := restarted.fake.Argv(t, 3); strings.Join(got, "\x00") != strings.Join(wantArgv, "\x00") {
		t.Errorf("after a restart codex ran\n  %q\nwant\n  %q", got, wantArgv)
	}

	// 4. Nobody else can read or run it. See the dedicated tests below.
}

// --- authorization ---------------------------------------------------------

func TestUnauthorizedUserGetsNothingAndRunsNothing(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("answer", "answer")})

	for i, text := range []string{"/help", "/new secret", "/sessions", "summarize the repo", "/stop"} {
		before := len(h.tg.Sent())
		h.say(msg(int64(100+i), aliceChat, malloryID, text))
		if after := len(h.tg.Sent()); after != before {
			t.Errorf("%q from an unauthorized user produced a reply: %q", text, h.tg.LastText())
		}
	}
	if n := len(h.fake.Invocations(t)); n != 0 {
		t.Errorf("codex was invoked %d time(s) for an unauthorized user", n)
	}
	if sessions := h.sessions(t, malloryID, true); len(sessions) != 0 {
		t.Errorf("an unauthorized user has %d session(s) stored", len(sessions))
	}
	// The updates were still claimed, so Telegram will not redeliver them
	// forever.
	for updateID := int64(100); updateID < 105; updateID++ {
		if claimed, err := h.st.UpdateClaimed(context.Background(), updateID); err != nil || !claimed {
			t.Errorf("update %d was not claimed: %v", updateID, err)
		}
	}
}

// TestUnauthorizedUserCannotReachASessionByID is the guessing test: a session id
// is not a capability.
func TestUnauthorizedUserCannotReachASessionByID(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("alice's answer", "alice's answer")})
	aliceSession := newSession(t, h, 1, aliceChat, aliceID, "alice work")
	h.text(msg(2, aliceChat, aliceID, "do something"))

	// Bob is allowlisted but owns nothing. Guessing alice's id must not work.
	for _, text := range []string{
		"/use " + aliceSession,
		"/session",
		"/rename " + aliceSession + " pwned",
		"/archive " + aliceSession,
	} {
		h.tg.Reset()
		reply := h.text(msg(3, bobChat, bobID, text))
		if strings.Contains(reply, aliceSession) && strings.Contains(reply, "Selected") {
			t.Errorf("%q let bob select alice's session: %q", text, reply)
		}
	}
	if got := h.text(msg(4, bobChat, bobID, "/sessions")); !strings.Contains(got, "no sessions") {
		t.Errorf("bob's /sessions leaked something: %q", got)
	}
	// And bob cannot run alice's session by sending a message: his own scope has
	// no selection, so he would get a session of his own.
	sess, err := h.st.GetSession(context.Background(), aliceSession, bobID)
	if err == nil {
		t.Errorf("bob can read alice's session: %+v", sess)
	}
	if _, err := h.st.GetSession(context.Background(), aliceSession, aliceID); err != nil {
		t.Errorf("alice lost her own session: %v", err)
	}

	// Alice's session is unchanged by all of it.
	if got, _ := h.st.GetSession(context.Background(), aliceSession, aliceID); got.Name != "alice work" {
		t.Errorf("alice's session was renamed to %q", got.Name)
	}
	if got, _ := h.st.GetSession(context.Background(), aliceSession, aliceID); got.Archived {
		t.Error("alice's session was archived by bob")
	}
}

func TestGroupAndChannelMessagesAreIgnored(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("answer", "answer")})

	h.say(testkit.GroupUpdate(1, -100222, aliceID, "/help"))
	channel := testkit.GroupUpdate(2, -100333, aliceID, "summarize")
	channel.Message.Chat.Type = "channel"
	h.say(channel)

	if n := len(h.tg.Sent()); n != 0 {
		t.Errorf("the bot answered %d message(s) in a group or channel", n)
	}
	if n := len(h.fake.Invocations(t)); n != 0 {
		t.Errorf("codex ran %d time(s) for a group message", n)
	}
	// Both updates were claimed so they are not redelivered.
	for _, id := range []int64{1, 2} {
		if ok, err := h.st.UpdateClaimed(context.Background(), id); err != nil || !ok {
			t.Errorf("update %d was not claimed: %v", id, err)
		}
	}
}

func TestBotSenderIsIgnored(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("answer", "answer")})
	u := msg(1, aliceChat, aliceID, "/help")
	u.Message.From.IsBot = true
	h.say(u)
	if len(h.tg.Sent()) != 0 {
		t.Error("the bot answered another bot")
	}
}

func TestNonTextMessageGetsAHint(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("answer", "answer")})
	u := msg(1, aliceChat, aliceID, "")
	got := h.text(u)
	if !strings.Contains(got, "text, photos, documents, audio and video") {
		t.Errorf("a message with no text got %q", got)
	}
	if n := len(h.fake.Invocations(t)); n != 0 {
		t.Errorf("codex ran for an empty message")
	}
}

// --- sessions --------------------------------------------------------------

func TestPlainMessageAutoCreatesASession(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("auto answer", "auto answer")})
	reply := h.text(msg(1, aliceChat, aliceID, "what is in this repo"))

	if !strings.Contains(reply, "No session was selected") {
		t.Errorf("the auto-created session was not announced:\n%s", reply)
	}
	if !strings.Contains(reply, "auto answer") {
		t.Errorf("the prompt was not answered:\n%s", reply)
	}
	sessions := h.sessions(t, aliceID, false)
	if len(sessions) != 1 {
		t.Fatalf("there are %d session(s), want the one that was created", len(sessions))
	}
	if !strings.HasPrefix(sessions[0].Name, "auto ") {
		t.Errorf("the automatic name is %q; it must not contain the user's text", sessions[0].Name)
	}
	if strings.Contains(sessions[0].Name, "repo") {
		t.Errorf("the user's message was copied into the stored name: %q", sessions[0].Name)
	}
	if cur := selected(t, h, aliceChat, 0, aliceID); cur != sessions[0].ID {
		t.Errorf("the automatic session was not selected")
	}
}

func TestSessionsListsOnlyTheCallersOwn(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("a", "a")})
	newSession(t, h, 1, aliceChat, aliceID, "alice one")
	newSession(t, h, 2, aliceChat, aliceID, "alice two")
	bobSession := newSession(t, h, 3, bobChat, bobID, "bob one")

	aliceList := h.text(msg(4, aliceChat, aliceID, "/sessions"))
	if !strings.Contains(aliceList, "alice one") || !strings.Contains(aliceList, "alice two") {
		t.Errorf("alice's list is missing her sessions:\n%s", aliceList)
	}
	if strings.Contains(aliceList, "bob one") || strings.Contains(aliceList, bobSession) {
		t.Errorf("alice can see bob's session:\n%s", aliceList)
	}
	if !strings.Contains(aliceList, "(2)") {
		t.Errorf("alice's list does not say how many she has:\n%s", aliceList)
	}

	bobList := h.text(msg(5, bobChat, bobID, "/sessions"))
	if !strings.Contains(bobList, "bob one") || strings.Contains(bobList, "alice") {
		t.Errorf("bob's list is wrong:\n%s", bobList)
	}
}

func TestSessionCommandShowsWorkspaceThreadAndStatus(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("answer", "answer")})
	if got := h.text(msg(1, aliceChat, aliceID, "/session")); !strings.Contains(got, "No session is selected") {
		t.Errorf("/session with nothing selected said %q", got)
	}
	id := newSession(t, h, 2, aliceChat, aliceID, "inspect")
	got := h.text(msg(3, aliceChat, aliceID, "/session"))
	for _, want := range []string{id, "inspect", h.cfg.Workspace, "none yet", "idle"} {
		if !strings.Contains(got, want) {
			t.Errorf("/session is missing %q:\n%s", want, got)
		}
	}
	h.text(msg(4, aliceChat, aliceID, "go"))
	got = h.text(msg(5, aliceChat, aliceID, "/session"))
	if !strings.Contains(got, threadOne) {
		t.Errorf("/session does not show the Codex thread after a turn:\n%s", got)
	}
	if !strings.Contains(got, "completed") {
		t.Errorf("/session does not show the last turn's status:\n%s", got)
	}
}

func TestArchiveHidesWithoutDeleting(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("answer", "answer")})
	id := newSession(t, h, 1, aliceChat, aliceID, "keepme")
	h.text(msg(2, aliceChat, aliceID, "do something"))

	got := h.text(msg(3, aliceChat, aliceID, "/archive "+id))
	if !strings.Contains(got, "Archived "+id) || !strings.Contains(got, "/unarchive") {
		t.Errorf("/archive replied %q", got)
	}
	if list := h.text(msg(4, aliceChat, aliceID, "/sessions")); strings.Contains(list, id) {
		t.Errorf("an archived session is still listed:\n%s", list)
	}
	if _, err := h.st.GetSelection(context.Background(), aliceChat, 0, aliceID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("archived session is still selected: %v", err)
	}
	if list := h.text(msg(5, aliceChat, aliceID, "/sessions all")); !strings.Contains(list, id) {
		t.Errorf("`/sessions all` does not list the archived session:\n%s", list)
	}
	// The Codex thread survives archiving: nothing was deleted, only hidden.
	sess, err := h.st.GetSession(context.Background(), id, aliceID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.CodexThreadID != threadOne {
		t.Errorf("archiving lost the thread id: %q", sess.CodexThreadID)
	}
	// And it can still be used.
	if got := h.text(msg(6, aliceChat, aliceID, "/use "+id)); !strings.Contains(got, "Selected") {
		t.Errorf("an archived session cannot be selected again: %q", got)
	}
	h.text(msg(7, aliceChat, aliceID, "/unarchive "+id))
	if list := h.text(msg(8, aliceChat, aliceID, "/sessions")); !strings.Contains(list, id) {
		t.Errorf("unarchiving did not restore the listing:\n%s", list)
	}
}

func TestRename(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("a", "a")})
	id := newSession(t, h, 1, aliceChat, aliceID, "before")

	if got := h.text(msg(2, aliceChat, aliceID, "/rename "+id+" after with spaces")); !strings.Contains(got, `"after with spaces"`) {
		t.Errorf("/rename replied %q", got)
	}
	sess, _ := h.st.GetSession(context.Background(), id, aliceID)
	if sess.Name != "after with spaces" {
		t.Errorf("Name = %q, want the whole remainder as the name", sess.Name)
	}
	if got := h.text(msg(3, aliceChat, aliceID, "/rename selected name with spaces")); !strings.Contains(got, `"selected name with spaces"`) {
		t.Errorf("selected /rename replied %q", got)
	}
	sess, _ = h.st.GetSession(context.Background(), id, aliceID)
	if sess.Name != "selected name with spaces" {
		t.Errorf("selected /rename set name to %q", sess.Name)
	}
	for i, bad := range []string{"/rename", "/rename " + id} {
		if got := h.text(msg(int64(30+i), aliceChat, aliceID, bad)); got == "" {
			t.Errorf("%q produced no answer at all", bad)
		}
	}
	sess, _ = h.st.GetSession(context.Background(), id, aliceID)
	if sess.Name != "selected name with spaces" {
		t.Errorf("a bad /rename changed the name to %q", sess.Name)
	}
	if got := h.text(msg(40, bobChat, bobID, "/rename absent selection")); !strings.Contains(got, "No session is selected") {
		t.Errorf("/rename without a selection replied %q", got)
	}
}

func TestRenameUsesSelectedTopicSession(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	first := sessionIDIn(h.text(testkit.TopicUpdate(1, aliceChat, 17, aliceID, "/new first")))
	second := sessionIDIn(h.text(testkit.TopicUpdate(2, aliceChat, 18, aliceID, "/new second")))
	if first == "" || second == "" {
		t.Fatalf("session ids = %q, %q", first, second)
	}
	if got := h.text(testkit.TopicUpdate(3, aliceChat, 17, aliceID, "/rename topic work")); !strings.Contains(got, "Renamed "+first) {
		t.Errorf("topic /rename replied %q", got)
	}
	firstSession, _ := h.st.GetSession(context.Background(), first, aliceID)
	secondSession, _ := h.st.GetSession(context.Background(), second, aliceID)
	if firstSession.Name != "topic work" || secondSession.Name != "second" {
		t.Errorf("topic rename changed names to %q and %q", firstSession.Name, secondSession.Name)
	}
}

func TestUUIDIsRejectedWhereASessionIDIsExpected(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("a", "a")})
	newSession(t, h, 1, aliceChat, aliceID, "x")
	got := h.text(msg(2, aliceChat, aliceID, "/use "+threadOne))
	if !strings.Contains(got, "UUID") {
		t.Errorf("passing a Codex UUID to /use said %q; it should explain the two id spaces", got)
	}
	// The UUID must not have reached a Codex command line.
	if n := len(h.fake.Invocations(t)); n != 0 {
		t.Errorf("codex was invoked %d time(s)", n)
	}
}

func TestUnknownCommand(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("a", "a")})
	got := h.text(msg(1, aliceChat, aliceID, "/frobnicate"))
	if !strings.Contains(got, "Unknown command") || !strings.Contains(got, "/help") {
		t.Errorf("an unknown command got %q", got)
	}
}

func TestHelpAndStart(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("a", "a")})
	for i, cmd := range []string{"/help", "/start"} {
		got := h.text(msg(int64(1+i), aliceChat, aliceID, cmd))
		for _, want := range []string{"/new", "/sessions", "/use", "/session", "/rename", "/archive", "/stop", "/help"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s does not document %s:\n%s", cmd, want, got)
			}
		}
		if !strings.Contains(got, h.cfg.Workspace) {
			t.Errorf("%s does not say which workspace is used:\n%s", cmd, got)
		}
		if !strings.Contains(got, "workspace-write") {
			t.Errorf("%s does not say which sandbox is in effect:\n%s", cmd, got)
		}
	}
}

func TestCommandAddressedToAnotherBotIsIgnored(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("a", "a")})
	if got := h.text(msg(1, aliceChat, aliceID, "/help@otherbot")); got != "" {
		t.Errorf("a command for another bot was answered: %q", got)
	}
	if got := h.text(msg(2, aliceChat, aliceID, "/help@"+h.tg.Username())); !strings.Contains(got, "/new") {
		t.Errorf("a command for this bot was not answered: %q", got)
	}
}

// --- topics ----------------------------------------------------------------

func TestSelectionIsScopedPerTopic(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("a", "a")})

	// Two topics in one private chat, one selection each.
	h.text(testkit.TopicUpdate(1, aliceChat, 10, aliceID, "/new topic-ten"))
	h.text(testkit.TopicUpdate(2, aliceChat, 20, aliceID, "/new topic-twenty"))

	ten := selected(t, h, aliceChat, 10, aliceID)
	twenty := selected(t, h, aliceChat, 20, aliceID)
	if ten == twenty {
		t.Fatalf("both topics point at the same session %q", ten)
	}
	// The chat itself, with no topic, has no selection: a topic selection must
	// not leak into the untopic'd scope or vice versa.
	if _, err := h.st.GetSelection(context.Background(), aliceChat, 0, aliceID); err == nil {
		t.Error("selecting inside a topic also selected a session for the chat without topics")
	}

	// /session in a topic reports that topic's session.
	if got := h.text(testkit.TopicUpdate(3, aliceChat, 10, aliceID, "/session")); !strings.Contains(got, ten) {
		t.Errorf("/session in topic 10 reported:\n%s", got)
	}
	if got := h.text(testkit.TopicUpdate(4, aliceChat, 20, aliceID, "/session")); !strings.Contains(got, twenty) {
		t.Errorf("/session in topic 20 reported:\n%s", got)
	}
	// A reply sent from a topic goes back to that topic.
	msgs := h.say(testkit.TopicUpdate(5, aliceChat, 10, aliceID, "/help"))
	for _, m := range msgs {
		if m.ThreadID != 10 {
			t.Errorf("a reply from topic 10 was sent to thread %d", m.ThreadID)
		}
	}
}

func TestStopIsScopedToTheChat(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: slowSpec()})
	newSession(t, h, 1, aliceChat, aliceID, "slow")

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.b.HandleUpdate(context.Background(), msg(2, aliceChat, aliceID, "a slow task"))
	}()
	waitForInflight(t, h, Scope{ChatID: aliceChat, UserID: aliceID})

	// A /stop from another user in another chat must not cancel alice's turn.
	if got := h.text(msg(3, bobChat, bobID, "/stop")); !strings.Contains(got, "Nothing is running") {
		t.Errorf("bob's /stop said %q", got)
	}
	if h.b.inflight.get(Scope{ChatID: aliceChat, UserID: aliceID}) == nil {
		t.Error("bob's /stop cancelled alice's turn")
	}

	h.text(msg(4, aliceChat, aliceID, "/stop"))
	<-done
	reply := h.tg.LastText()
	if !strings.Contains(reply, "Cancelled") {
		t.Errorf("after /stop the reply was %q", reply)
	}
}

// --- cancellation ----------------------------------------------------------

func TestStopCancelsTheTurnButKeepsTheSession(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: slowSpec()})
	id := newSession(t, h, 1, aliceChat, aliceID, "cancellable")

	done := make(chan struct{})
	go func() {
		defer close(done)
		h.b.HandleUpdate(context.Background(), msg(2, aliceChat, aliceID, "a long task"))
	}()
	waitForInflight(t, h, Scope{ChatID: aliceChat, UserID: aliceID})
	// Wait until Codex has actually reported its thread before cancelling.
	// Without this, /stop can land before the child has printed anything, and
	// "the session kept its thread id" would be asserting something that never
	// had a chance to happen — which is a test race, not a bot behaviour.
	h.fake.WaitForEmitted(t, 1, 15*time.Second)

	h.tg.Reset()
	if got := h.text(msg(3, aliceChat, aliceID, "/stop")); !strings.Contains(got, "Cancelling") {
		t.Errorf("/stop replied %q", got)
	}
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the turn did not stop after /stop")
	}

	reply := h.tg.LastText()
	if !strings.Contains(reply, "Cancelled") {
		t.Errorf("the turn's final message was %q", reply)
	}
	if strings.Contains(reply, "never delivered") {
		t.Error("a cancelled turn delivered its partial answer")
	}
	turn := h.lastTurn(t, id)
	if turn.Status != store.TurnCancelled {
		t.Errorf("the turn status is %q, want cancelled", turn.Status)
	}
	// The session itself is untouched and still resumable: /stop cancels a turn,
	// not the conversation.
	sess, err := h.st.GetSession(context.Background(), id, aliceID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.CodexThreadID != threadOne {
		t.Errorf("cancelling lost the thread id: %q", sess.CodexThreadID)
	}
	// Nothing is left in flight, so the next message can run.
	if h.b.inflight.get(Scope{ChatID: aliceChat, UserID: aliceID}) != nil {
		t.Error("the in-flight entry was not cleared after cancellation")
	}
	if held := h.b.wsLocks.Held(workspaceKey(h.cfg.Workspace)); held {
		t.Error("the workspace lock was not released after cancellation")
	}
}

func TestStopWithNothingRunning(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("a", "a")})
	if got := h.text(msg(1, aliceChat, aliceID, "/stop")); !strings.Contains(got, "Nothing is running") {
		t.Errorf("/stop with no turn said %q", got)
	}
}

// --- deduplication ---------------------------------------------------------

func TestDuplicateUpdateRunsCodexOnce(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("the answer", "the answer")})
	id := newSession(t, h, 1, aliceChat, aliceID, "dedup")

	update := msg(2, aliceChat, aliceID, "do the thing")
	first := h.text(update)
	if first != "the answer" {
		t.Fatalf("the first delivery replied %q", first)
	}

	// Telegram redelivers the same update after a crash between claiming it and
	// advancing the offset. The reply was delivered, so the correct answer now is
	// silence — and above all not a second Codex run.
	h.tg.Reset()
	h.say(update)
	if n := len(h.tg.Sent()); n != 0 {
		t.Errorf("a duplicate update produced %d message(s): %q", n, h.tg.AllText())
	}
	if n := len(h.fake.Invocations(t)); n != 1 {
		t.Errorf("codex was invoked %d time(s) for one message; /new does not run codex", n)
	}
	turns, err := countTurns(h, id)
	if err != nil {
		t.Fatalf("count turns: %v", err)
	}
	if turns != 1 {
		t.Errorf("there are %d turn rows for one message", turns)
	}
}

// TestUndeliveredReplyIsResentNotRecomputed covers the window that cannot be
// closed: the process died after Codex finished but before Telegram accepted the
// reply.
func TestUndeliveredReplyIsResentNotRecomputed(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("the computed answer", "the computed answer")})
	id := newSession(t, h, 1, aliceChat, aliceID, "recover")

	update := msg(2, aliceChat, aliceID, "do the thing")
	if got := h.text(update); got != "the computed answer" {
		t.Fatalf("the first delivery replied %q", got)
	}
	// Simulate the crash: the reply exists but its delivery was never confirmed.
	if _, err := h.st.DB().ExecContext(context.Background(),
		`UPDATE turns SET delivered = 0 WHERE session_id = ?`, id); err != nil {
		t.Fatalf("clear the delivered flag: %v", err)
	}

	h.tg.Reset()
	h.say(update)
	if got := h.tg.AllText(); got != "the computed answer" {
		t.Errorf("the undelivered reply was not resent: %q", got)
	}
	if n := len(h.fake.Invocations(t)); n != 1 {
		t.Errorf("codex ran %d time(s); recovery must not recompute an answer it already has", n)
	}
	turn := h.lastTurn(t, id)
	if !turn.Delivered {
		t.Error("the resent reply was not marked delivered")
	}

	// And a third delivery of the same update is silence again.
	h.tg.Reset()
	h.say(update)
	if n := len(h.tg.Sent()); n != 0 {
		t.Errorf("a third delivery produced %d message(s)", n)
	}
}

func TestSavedReplyIsRetriedAfterOffsetAdvances(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("saved answer", "saved answer")})
	h.say(testkit.TopicUpdate(1, aliceChat, 17, aliceID, "/new saved"))
	h.tg.Reset()
	h.tg.SetSendError(errors.New("temporary Telegram failure"))
	update := testkit.TopicUpdate(2, aliceChat, 17, aliceID, "do the thing")
	h.say(update)
	if len(h.tg.Sent()) != 0 {
		t.Fatal("Telegram accepted a reply during the simulated failure")
	}
	id := selected(t, h, aliceChat, 17, aliceID)
	if turn := h.lastTurn(t, id); turn.Status != store.TurnCompleted || turn.Delivered {
		t.Fatalf("failed delivery was not saved for retry: %+v", turn)
	}
	if err := h.st.AdvanceOffset(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.DB().ExecContext(context.Background(),
		`UPDATE turns SET finished_at = ? WHERE update_id = ?`,
		time.Now().Add(-3*time.Minute).UTC().Format(time.RFC3339Nano), 2); err != nil {
		t.Fatal(err)
	}
	h.tg.SetSendError(nil)
	h.b.retryPendingReplies(context.Background())
	sent := h.tg.Sent()
	if len(sent) != 1 || sent[0].Text != "saved answer" || sent[0].ThreadID != 17 {
		t.Fatalf("retried reply = %+v", sent)
	}
	if n := len(h.fake.Invocations(t)); n != 1 {
		t.Fatalf("Codex ran %d times; retry must only send the saved answer", n)
	}
	h.b.retryPendingReplies(context.Background())
	if len(h.tg.Sent()) != 1 {
		t.Fatal("a confirmed answer was retried again")
	}
}

func TestLegacySavedReplyFallsBackToRecordedPrivateChat(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("old answer", "old answer")})
	h.say(testkit.TopicUpdate(1, aliceChat, 17, aliceID, "/new old"))
	id := selected(t, h, aliceChat, 17, aliceID)
	h.tg.Reset()
	h.tg.SetSendError(errors.New("temporary Telegram failure"))
	h.say(testkit.TopicUpdate(2, aliceChat, 17, aliceID, "old prompt"))
	h.tg.SetSendError(nil)
	if _, err := h.st.DB().ExecContext(context.Background(),
		`UPDATE processed_updates SET thread_id = NULL WHERE update_id = 2`); err != nil {
		t.Fatal(err)
	}
	if err := h.st.ClearSelection(context.Background(), aliceChat, 17, aliceID); err != nil {
		t.Fatal(err)
	}
	if _, err := h.st.DB().ExecContext(context.Background(),
		`UPDATE turns SET finished_at = ? WHERE update_id = 2`,
		time.Now().Add(-3*time.Minute).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	h.b.retryPendingReplies(context.Background())
	sent := h.tg.Sent()
	if len(sent) != 1 || sent[0].ChatID != aliceChat || sent[0].ThreadID != 0 ||
		!strings.Contains(sent[0].Text, "original topic unavailable") ||
		!strings.Contains(sent[0].Text, id) || !strings.Contains(sent[0].Text, "old answer") {
		t.Fatalf("legacy recovery message = %+v", sent)
	}
	if turn := h.lastTurn(t, id); !turn.Delivered {
		t.Fatalf("legacy recovery was not marked delivered: %+v", turn)
	}
	if n := len(h.fake.Invocations(t)); n != 1 {
		t.Fatalf("legacy recovery reran Codex %d times", n)
	}
}

func TestPollDispatchOrdersSessionSwitchAfterTurn(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: slowSpec()})
	oldID := newSession(t, h, 1, aliceChat, aliceID, "old")
	for _, update := range []telegram.Update{
		msg(2, aliceChat, aliceID, "slow prompt"),
		msg(3, aliceChat, aliceID, "/new next"),
	} {
		p, err := h.b.Claim(context.Background(), update)
		if err != nil {
			t.Fatal(err)
		}
		h.b.dispatch(context.Background(), p)
	}
	h.fake.WaitForEmitted(t, 1, 15*time.Second)
	if got := selected(t, h, aliceChat, 0, aliceID); got != oldID {
		t.Fatalf("/new overtook the earlier prompt: selected %s, want %s", got, oldID)
	}
	stop, err := h.b.Claim(context.Background(), msg(4, aliceChat, aliceID, "/stop"))
	if err != nil {
		t.Fatal(err)
	}
	h.b.dispatch(context.Background(), stop)
	h.b.Wait()
	if got := selected(t, h, aliceChat, 0, aliceID); got == oldID {
		t.Fatal("/new never ran after the active turn finished")
	}
	if turn := h.lastTurn(t, oldID); turn.Status != store.TurnCancelled {
		t.Fatalf("earlier prompt ran in the wrong session or was not cancelled: %+v", turn)
	}
}

func TestStopTargetsActiveTurnBeforeQueuedTurn(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: slowSpec(), env: map[string]string{"BOT_QUEUE_TIMEOUT": "30s"}})
	id := newSession(t, h, 1, aliceChat, aliceID, "stop queue")
	for _, update := range []telegram.Update{
		msg(2, aliceChat, aliceID, "first slow prompt"),
		msg(3, aliceChat, aliceID, "second slow prompt"),
	} {
		p, err := h.b.Claim(context.Background(), update)
		if err != nil {
			t.Fatal(err)
		}
		h.b.dispatch(context.Background(), p)
	}
	h.fake.WaitForEmitted(t, 1, 15*time.Second)
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(h.tg.AllText(), "Queued behind an earlier message") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(h.tg.AllText(), "Queued behind an earlier message") {
		t.Fatal("the queued turn was not acknowledged")
	}
	for _, updateID := range []int64{4, 5} {
		if updateID == 5 {
			h.fake.WaitForEmitted(t, 2, 15*time.Second)
		}
		p, err := h.b.Claim(context.Background(), msg(updateID, aliceChat, aliceID, "/stop"))
		if err != nil {
			t.Fatal(err)
		}
		h.b.dispatch(context.Background(), p)
	}
	h.b.Wait()
	rows, err := h.st.DB().QueryContext(context.Background(),
		`SELECT status FROM turns WHERE session_id = ? ORDER BY update_id`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var statuses []string
	for rows.Next() {
		var status string
		if err := rows.Scan(&status); err != nil {
			t.Fatal(err)
		}
		statuses = append(statuses, status)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 2 || statuses[0] != store.TurnCancelled || statuses[1] != store.TurnCancelled {
		t.Fatalf("turn statuses after consecutive /stop commands: %v", statuses)
	}
}

func TestSessionReportsActiveTurnBeforeItFinishes(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: slowSpec()})
	newSession(t, h, 1, aliceChat, aliceID, "live status")
	turn, err := h.b.Claim(context.Background(), msg(2, aliceChat, aliceID, "slow task"))
	if err != nil {
		t.Fatal(err)
	}
	h.b.dispatch(context.Background(), turn)
	h.fake.WaitForEmitted(t, 1, 15*time.Second)
	status, err := h.b.Claim(context.Background(), msg(3, aliceChat, aliceID, "/session"))
	if err != nil {
		t.Fatal(err)
	}
	h.b.dispatch(context.Background(), status)
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(h.tg.AllText(), "Status: running") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(h.tg.AllText(), "Status: running") {
		t.Fatal("/session waited behind the active turn")
	}
	stop, err := h.b.Claim(context.Background(), msg(4, aliceChat, aliceID, "/stop"))
	if err != nil {
		t.Fatal(err)
	}
	h.b.dispatch(context.Background(), stop)
	h.b.Wait()
}

func TestStopBeforeSessionAssignmentHasCompleteMessage(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	scope := Scope{ChatID: aliceChat, UserID: aliceID}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entry := &inflightTurn{cancel: cancel, started: time.Now()}
	h.b.inflight.register(scope, entry)
	defer h.b.inflight.clear(scope, entry)
	h.b.cmdStop(context.Background(), &Prepared{Scope: scope})
	if got := h.tg.AllText(); !strings.Contains(got, "No session has been assigned") {
		t.Fatalf("/stop before session assignment replied %q", got)
	}
	if ctx.Err() == nil {
		t.Fatal("/stop did not cancel the queued turn")
	}
}

func TestDispatchQueueTimeoutIncludesScopeWait(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: slowSpec(), env: map[string]string{"BOT_QUEUE_TIMEOUT": "150ms"}})
	newSession(t, h, 1, aliceChat, aliceID, "queue timeout")
	first, err := h.b.Claim(context.Background(), msg(2, aliceChat, aliceID, "first prompt"))
	if err != nil {
		t.Fatal(err)
	}
	h.b.dispatch(context.Background(), first)
	h.fake.WaitForEmitted(t, 1, 15*time.Second)
	second, err := h.b.Claim(context.Background(), msg(3, aliceChat, aliceID, "second prompt"))
	if err != nil {
		t.Fatal(err)
	}
	h.b.dispatch(context.Background(), second)
	deadline := time.Now().Add(3 * time.Second)
	for !strings.Contains(h.tg.AllText(), "wait limit (150ms) expired") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(h.tg.AllText(), "wait limit (150ms) expired") {
		t.Fatal("queued prompt did not report its scope wait timeout")
	}
	stop, err := h.b.Claim(context.Background(), msg(4, aliceChat, aliceID, "/stop"))
	if err != nil {
		t.Fatal(err)
	}
	h.b.dispatch(context.Background(), stop)
	h.b.Wait()
	if n := len(h.fake.Invocations(t)); n != 1 {
		t.Fatalf("Codex ran %d turns; timed-out prompt must not run", n)
	}
}

func TestConcurrentTurnAdmissionRespectsLimit(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.cfg.MaxConcurrent = 2
	const callers = 32
	var wg sync.WaitGroup
	results := make(chan bool, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- h.b.reserveTurn()
		}()
	}
	wg.Wait()
	close(results)
	var admitted int
	for ok := range results {
		if ok {
			admitted++
		}
	}
	if admitted != h.cfg.MaxConcurrent {
		t.Fatalf("admitted %d turns with limit %d", admitted, h.cfg.MaxConcurrent)
	}
}

func TestDuplicateCommandIsNotRepeated(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("a", "a")})
	update := msg(1, aliceChat, aliceID, "/new once")
	if got := h.text(update); !strings.Contains(got, "Created session") {
		t.Fatalf("/new replied %q", got)
	}
	h.tg.Reset()
	h.say(update)
	if n := len(h.tg.Sent()); n != 0 {
		t.Errorf("a duplicate /new sent %d message(s)", n)
	}
	if sessions := h.sessions(t, aliceID, true); len(sessions) != 1 {
		t.Errorf("a duplicate /new created %d session(s), want 1", len(sessions))
	}
}

// --- serialization ---------------------------------------------------------

func TestTurnsSharingAWorktreeNeverOverlap(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: testkit.CodexSpec{
		Stdout: testkit.Events(threadOne, "answer"), SleepBefore: 400 * time.Millisecond,
	}, env: map[string]string{"BOT_QUEUE_TIMEOUT": "30s"}})
	newSession(t, h, 1, aliceChat, aliceID, "serial")

	const n = 3
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h.b.HandleUpdate(context.Background(),
				msg(int64(10+i), aliceChat, aliceID, "task "+string(rune('a'+i))))
		}(i)
	}
	wg.Wait()

	invocations := h.fake.Invocations(t)
	if len(invocations) != n {
		t.Fatalf("codex ran %d time(s), want %d", len(invocations), n)
	}
	timeline := h.fake.Timeline(t)
	if len(timeline) != n {
		t.Fatalf("the timeline has %d complete interval(s), want %d", len(timeline), n)
	}
	if a, b, overlapping := testkit.Overlaps(timeline); overlapping {
		t.Errorf("turn %d (%s..%s) overlapped turn %d (%s..%s): two Codex processes shared the worktree",
			a.N, a.Start.Format(time.StampNano), a.End.Format(time.StampNano),
			b.N, b.Start.Format(time.StampNano), b.End.Format(time.StampNano))
	}
	// Every message still got an answer: serialization must not drop work.
	if got := len(h.tg.Sent()); got < n {
		t.Errorf("only %d message(s) were sent for %d prompts", got, n)
	}
}

func TestBusyWhenTheWorkspaceIsHeldBySomethingElse(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("a", "a")})
	id := newSession(t, h, 1, aliceChat, aliceID, "blocked")

	// Hold the workspace lock the way another chat's turn would.
	release, err := h.b.wsLocks.Acquire(context.Background(), workspaceKey(h.cfg.Workspace))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	start := time.Now()
	reply := h.text(msg(2, aliceChat, aliceID, "please run"))
	release()

	if !strings.Contains(reply, "in use") || !strings.Contains(reply, "/stop") {
		t.Errorf("a blocked turn replied %q", reply)
	}
	if time.Since(start) < h.cfg.QueueTimeout {
		t.Errorf("the busy reply came after %s, before the %s queue limit expired",
			time.Since(start), h.cfg.QueueTimeout)
	}
	if n := len(h.fake.Invocations(t)); n != 0 {
		t.Errorf("codex ran %d time(s) while the workspace was held", n)
	}
	turn := h.lastTurn(t, id)
	if turn.Status != store.TurnBusy {
		t.Errorf("the turn status is %q, want busy", turn.Status)
	}
	// The message was consumed, not left to be retried forever.
	if ok, _ := h.st.UpdateClaimed(context.Background(), 2); !ok {
		t.Error("a busy update was not claimed")
	}
}

func TestConcurrentTurnsAreAdmittedUpToTheLimit(t *testing.T) {
	h := newHarness(t, harnessOpts{
		spec: testkit.CodexSpec{Stdout: testkit.Events(threadOne, "a"), SleepBefore: 300 * time.Millisecond},
		env:  map[string]string{"BOT_MAX_CONCURRENT_UPDATES": "1", "BOT_QUEUE_TIMEOUT": "30s"},
	})
	newSession(t, h, 1, aliceChat, aliceID, "limited")

	// Hold the one allowed slot, then send another message: it must be refused
	// immediately with a clear answer rather than queueing without bound.
	h.b.pendingTurns.Add(1)
	defer h.b.pendingTurns.Add(-1)

	start := time.Now()
	reply := h.text(msg(2, aliceChat, aliceID, "another task"))
	if !strings.Contains(reply, "limit") {
		t.Errorf("the over-limit reply was %q", reply)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("the refusal took %s; it should not wait", time.Since(start))
	}
}

// --- output ----------------------------------------------------------------

func TestLongReplyIsSplitBelowTheTelegramLimit(t *testing.T) {
	long := strings.Repeat("The answer is long. ", 900) // ~18000 characters
	h := newHarness(t, harnessOpts{spec: successSpec(long, long)})
	newSession(t, h, 1, aliceChat, aliceID, "verbose")

	msgs := h.say(msg(2, aliceChat, aliceID, "tell me everything"))
	if len(msgs) < 2 {
		t.Fatalf("a %d-character reply was sent as %d message(s)", len(long), len(msgs))
	}
	var rejoined strings.Builder
	for i, m := range msgs {
		if u := textsplit.Units(m.Text); u > textsplit.MaxText {
			t.Errorf("chunk %d has %d UTF-16 units, over Telegram's %d limit", i, u, textsplit.MaxText)
		}
		rejoined.WriteString(m.Text)
	}
	if strings.Join(strings.Fields(rejoined.String()), "") != strings.Join(strings.Fields(long), "") {
		t.Error("the reassembled reply is not the text Codex produced")
	}
	turn := h.lastTurn(t, sessionsOf(t, h, aliceID)[0].ID)
	if !turn.Delivered {
		t.Error("a fully delivered split reply was not marked delivered")
	}
}

func TestFailureIsReportedWithoutSecrets(t *testing.T) {
	stderr := strings.Join([]string{
		"2026-09-25T10:00:00Z ERROR codex_core: the model request failed",
		"token: sk-SuperSecretKeyValue1234567890",
		"Authorization: Bearer eyJhbGciOiJIUzI1.secret-part",
		"OPENAI_API_KEY=sk-anotherSecretValue123456",
		"Error: something finally went wrong",
	}, "\n")
	h := newHarness(t, harnessOpts{spec: testkit.CodexSpec{Stderr: stderr, Exit: 1}})
	newSession(t, h, 1, aliceChat, aliceID, "broken")

	reply := h.text(msg(2, aliceChat, aliceID, "do something"))
	for _, secret := range []string{
		"SuperSecretKeyValue", "eyJhbGciOiJIUzI1", "anotherSecretValue",
		"AAtest-token-value", "--strict-config", "sandbox_mode",
	} {
		if strings.Contains(reply, secret) {
			t.Errorf("the failure message leaked %q:\n%s", secret, reply)
		}
	}
	if !strings.Contains(reply, "failed") {
		t.Errorf("the failure message does not say the turn failed:\n%s", reply)
	}
	if !strings.Contains(reply, "something finally went wrong") {
		t.Errorf("the failure message dropped the useful part of stderr:\n%s", reply)
	}
	if !strings.Contains(reply, "[redacted]") {
		t.Errorf("the secrets were dropped silently instead of being marked:\n%s", reply)
	}
}

func TestMissingThreadIsExplained(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: testkit.CodexSpec{
		Stderr: "Error: thread/resume: thread/resume failed: no rollout found for thread id " +
			threadOne + " (code -32600)\n",
		Exit: 1,
	}})
	id := newSession(t, h, 1, aliceChat, aliceID, "orphaned")
	// Give the session a thread id that Codex no longer has.
	if err := h.st.SetThreadID(context.Background(), id, aliceID, threadOne); err != nil {
		t.Fatalf("SetThreadID: %v", err)
	}
	reply := h.text(msg(2, aliceChat, aliceID, "continue please"))
	if !strings.Contains(reply, "no longer has the conversation") || !strings.Contains(reply, "/new") {
		t.Errorf("a lost thread was not explained:\n%s", reply)
	}
}

func TestTimeoutIsReported(t *testing.T) {
	h := newHarness(t, harnessOpts{
		spec: testkit.CodexSpec{Stdout: testkit.Events(threadOne, "late"), SleepBefore: 20 * time.Second},
		env:  map[string]string{"BOT_TURN_TIMEOUT": "30s"},
	})
	// Overriding the whole timeout would make the test slow; drive the runner's
	// own deadline instead.
	h.runner = mustRunnerWithTimeout(t, h, 700*time.Millisecond)
	h.b.cx = h.runner

	newSession(t, h, 1, aliceChat, aliceID, "slow")
	id := sessionsOf(t, h, aliceID)[0].ID

	start := time.Now()
	reply := h.text(msg(2, aliceChat, aliceID, "a task that never ends"))
	if !strings.Contains(reply, "did not finish within") || !strings.Contains(reply, "BOT_TURN_TIMEOUT") {
		t.Errorf("the timeout was not explained:\n%s", reply)
	}
	if time.Since(start) > 15*time.Second {
		t.Errorf("the timeout took %s to take effect", time.Since(start))
	}
	if turn := h.lastTurn(t, id); turn.Status != store.TurnTimeout {
		t.Errorf("the turn status is %q, want timeout", turn.Status)
	}
}

// --- restart and recovery --------------------------------------------------

func TestInterruptedTurnIsVisibleAfterACrash(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("answer", "answer")})
	id := newSession(t, h, 1, aliceChat, aliceID, "crashed")

	// A turn row left 'running' by a process that died.
	turnID, err := h.st.BeginTurn(context.Background(), 99, id, aliceID, 5, []string{"exec", "--json"})
	if err != nil {
		t.Fatalf("BeginTurn: %v", err)
	}
	h.Close()

	restarted := newHarness(t, harnessOpts{ws: h.ws, state: h.state, home: h.home,
		spec: successSpec("answer", "answer")})

	// This is what main does at startup.
	n, err := restarted.st.MarkInterruptedTurns(context.Background())
	if err != nil {
		t.Fatalf("MarkInterruptedTurns: %v", err)
	}
	if n != 1 {
		t.Errorf("MarkInterruptedTurns = %d, want 1", n)
	}
	got, err := restarted.st.TurnByUpdate(context.Background(), 99)
	if err != nil {
		t.Fatalf("TurnByUpdate: %v", err)
	}
	if got.ID != turnID || got.Status != store.TurnInterrupted {
		t.Errorf("turn = %+v, want %d interrupted", got, turnID)
	}

	// And the user can see it without reading the journal.
	report := restarted.text(msg(100, aliceChat, aliceID, "/session"))
	if !strings.Contains(report, "interrupted") || !strings.Contains(report, "restart") {
		t.Errorf("/session does not report the interrupted turn:\n%s", report)
	}
	if !strings.Contains(report, "git status") {
		t.Errorf("/session does not warn about partial edits:\n%s", report)
	}
	// No prompt was silently re-issued by the restart.
	if n := len(restarted.fake.Invocations(t)); n != 0 {
		t.Errorf("restarting invoked codex %d time(s); it must not replay a prompt", n)
	}
}

func TestSelectionAndThreadSurviveARestart(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("first answer", "resumed answer")})
	id := newSession(t, h, 1, aliceChat, aliceID, "persistent")
	if got := h.text(msg(2, aliceChat, aliceID, "remember this")); got != "first answer" {
		t.Fatalf("the first turn replied %q", got)
	}
	h.Close()

	// Same workspace, state directory and CODEX_HOME: a new process, same data.
	restarted := newHarness(t, harnessOpts{ws: h.ws, state: h.state, home: h.home, fake: h.fake})

	if cur := selected(t, restarted, aliceChat, 0, aliceID); cur != id {
		t.Errorf("after a restart the selection is %q, want %q", cur, id)
	}
	if got := restarted.text(msg(3, aliceChat, aliceID, "and now this")); got != "resumed answer" {
		t.Errorf("after a restart the reply was %q, want the resumed answer", got)
	}
	argv := restarted.fake.Argv(t, 2)
	if len(argv) < 3 || argv[1] != "resume" || argv[2] != threadOne {
		t.Errorf("after a restart codex ran %q, want a resume of %q", argv, threadOne)
	}
}

func TestOffsetSurvivesARestart(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("a", "a")})
	newSession(t, h, 1, aliceChat, aliceID, "offset")
	if err := h.st.AdvanceOffset(context.Background(), 1234); err != nil {
		t.Fatalf("AdvanceOffset: %v", err)
	}
	h.Close()

	restarted := newHarness(t, harnessOpts{ws: h.ws, state: h.state, home: h.home,
		spec: successSpec("a", "a")})
	got, err := restarted.st.GetOffset(context.Background())
	if err != nil {
		t.Fatalf("GetOffset: %v", err)
	}
	if got != 1234 {
		t.Errorf("after a restart the offset is %d, want 1234", got)
	}
}

// --- logging ---------------------------------------------------------------

func TestPromptsAreNotLoggedByDefault(t *testing.T) {
	var buf bytes.Buffer
	h := newHarness(t, harnessOpts{
		spec:    successSpec("answer", "answer"),
		logSink: &buf,
	})
	const secretPrompt = "the-prompts-that-must-not-be-logged"
	newSession(t, h, 1, aliceChat, aliceID, "quiet")
	h.text(msg(2, aliceChat, aliceID, secretPrompt))

	logged := buf.String()
	if strings.Contains(logged, secretPrompt) {
		t.Errorf("the prompt reached the log:\n%s", logged)
	}
	if !strings.Contains(logged, "prompt_chars") {
		t.Errorf("the log carries no length either, so nothing at all was recorded:\n%s", logged)
	}
	// The stored turn must not contain the prompt either.
	turn := h.lastTurn(t, sessionsOf(t, h, aliceID)[0].ID)
	if strings.Contains(turn.Reply, secretPrompt) {
		t.Error("the prompt was stored in the turn row")
	}
	for _, arg := range turn.Argv {
		if strings.Contains(arg, secretPrompt) {
			t.Errorf("the stored argv contains the prompt: %q", turn.Argv)
		}
	}
	if !strings.Contains(strings.Join(turn.Argv, " "), "<prompt>") {
		t.Errorf("the stored argv has no placeholder: %q", turn.Argv)
	}
}

func TestPromptsAreLoggedWhenAskedFor(t *testing.T) {
	var buf bytes.Buffer
	h := newHarness(t, harnessOpts{
		spec:    successSpec("answer", "answer"),
		logSink: &buf,
		env:     map[string]string{"BOT_LOG_PROMPTS": "true"},
	})
	const prompt = "a-prompt-i-chose-to-log"
	newSession(t, h, 1, aliceChat, aliceID, "loud")
	h.text(msg(2, aliceChat, aliceID, prompt))
	if !strings.Contains(buf.String(), prompt) {
		t.Errorf("BOT_LOG_PROMPTS=true did not log the prompt:\n%s", buf.String())
	}
}

func TestTypingIndicatorIsSent(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("answer", "answer")})
	newSession(t, h, 1, aliceChat, aliceID, "typing")
	h.text(msg(2, aliceChat, aliceID, "hello"))
	actions := h.tg.Actions()
	if len(actions) == 0 {
		t.Fatal("no typing indicator was sent while a turn ran")
	}
	for _, a := range actions {
		if a != "typing" {
			t.Errorf("chat action %q is not \"typing\"", a)
		}
	}
}

func TestAckIsSentForASlowTurn(t *testing.T) {
	h := newHarness(t, harnessOpts{
		spec: testkit.CodexSpec{Stdout: testkit.Events(threadOne, "finally"), SleepBefore: 900 * time.Millisecond},
		env:  map[string]string{"BOT_ACK_AFTER": "100ms"},
	})
	newSession(t, h, 1, aliceChat, aliceID, "ackme")
	h.text(msg(2, aliceChat, aliceID, "a slow task"))

	all := h.tg.AllText()
	if !strings.Contains(all, "Working on it") || !strings.Contains(all, "/stop") {
		t.Errorf("no acknowledgement was sent for a slow turn:\n%s", all)
	}
	if !strings.Contains(all, "finally") {
		t.Errorf("the final answer never arrived:\n%s", all)
	}
}

func TestNoAckForAFastTurn(t *testing.T) {
	h := newHarness(t, harnessOpts{
		spec: successSpec("quick", "quick"),
		env:  map[string]string{"BOT_ACK_AFTER": "30s"},
	})
	newSession(t, h, 1, aliceChat, aliceID, "fast")
	h.text(msg(2, aliceChat, aliceID, "a fast task"))
	if strings.Contains(h.tg.AllText(), "Working on it") {
		t.Error("an acknowledgement was sent for a turn that finished immediately")
	}
}

// --- poller ----------------------------------------------------------------

func TestPollAdvancesTheOffsetOnlyAfterClaiming(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("answer", "answer")})
	h.tg.QueueUpdates(
		msg(10, aliceChat, aliceID, "/help"),
		msg(11, aliceChat, aliceID, "/sessions"),
		msg(12, bobChat, bobID, "/help"),
	)

	ctx, cancel := context.WithCancel(context.Background())
	polled := make(chan error, 1)
	go func() { polled <- h.b.Poll(ctx) }()

	// Wait for all three to be claimed and the offset to pass them.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if off, _ := h.st.GetOffset(context.Background()); off >= 13 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-polled

	if off, err := h.st.GetOffset(context.Background()); err != nil || off != 13 {
		t.Errorf("the offset is %d (%v), want 13", off, err)
	}
	for _, id := range []int64{10, 11, 12} {
		if ok, err := h.st.UpdateClaimed(context.Background(), id); err != nil || !ok {
			t.Errorf("update %d was not claimed: %v", id, err)
		}
	}
	h.b.Wait()
	if n := len(h.tg.Sent()); n == 0 {
		t.Error("the polled updates produced no replies")
	}
}

func TestPollStopsOnABadToken(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("a", "a")})
	h.tg.SetPollError(&telegram.APIError{Method: "getUpdates", ErrorCode: 401, Description: "Unauthorized"})

	err := h.b.Poll(context.Background())
	if err == nil {
		t.Fatal("Poll kept running with a rejected token")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("the error does not name the cause: %v", err)
	}
}

func TestPrepareClearsTheWebhook(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("a", "a")})
	if err := h.b.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	info, err := h.tg.GetWebhookInfo(context.Background())
	if err != nil {
		t.Fatalf("GetWebhookInfo: %v", err)
	}
	if info.URL != "" {
		t.Errorf("the webhook is still set to %q; a poller cannot work while it is", info.URL)
	}
	if h.b.botUsername != h.tg.Username() {
		t.Errorf("the bot username is %q, want %q", h.b.botUsername, h.tg.Username())
	}
}

// --- helpers ---------------------------------------------------------------

func selected(t *testing.T, h *harness, chatID, threadID, userID int64) string {
	t.Helper()
	id, err := h.st.GetSelection(context.Background(), chatID, threadID, userID)
	if err != nil {
		t.Fatalf("GetSelection(%d,%d,%d): %v", chatID, threadID, userID, err)
	}
	return id
}

func sessionsOf(t *testing.T, h *harness, owner int64) []store.Session {
	t.Helper()
	return h.sessions(t, owner, true)
}

func countTurns(h *harness, sessionID string) (int, error) {
	var n int
	err := h.st.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM turns WHERE session_id = ?`, sessionID).Scan(&n)
	return n, err
}

func waitForInflight(t *testing.T, h *harness, scope Scope) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if h.b.inflight.get(scope) != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no turn was registered as in flight")
}

func TestAuthFailureIsExplained(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: testkit.CodexSpec{
		Stderr: "Error: not logged in\n", Exit: 1,
	}})
	newSession(t, h, 1, aliceChat, aliceID, "unauthenticated")
	reply := h.text(msg(2, aliceChat, aliceID, "do something"))
	if !strings.Contains(reply, "login --device-auth") {
		t.Errorf("an unauthenticated Codex was not explained with the fix:\n%s", reply)
	}
}

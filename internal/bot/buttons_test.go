package bot

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"codex-telegram-bot/internal/telegram"
	"codex-telegram-bot/internal/testkit"
)

func buttonUpdate(updateID, chatID, threadID, userID int64, data string) telegram.Update {
	return telegram.Update{UpdateID: updateID, CallbackQuery: &telegram.CallbackQuery{
		ID: "cb-" + itoa(updateID), Data: data,
		From: &telegram.User{ID: userID},
		Message: &telegram.Message{MessageID: 1000, Date: time.Now().Unix(),
			MessageThreadID: threadID, Chat: telegram.Chat{ID: chatID, Type: "private"}},
	}}
}

func TestPrepareRegistersTelegramCommandMenu(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	if err := h.b.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	commands := h.tg.Commands()
	if len(commands) != 10 || commands[0].Command != "start" || commands[5].Command != "rename" || commands[6].Command != "archive" || commands[9].Command != "stop" {
		t.Fatalf("registered commands = %+v", commands)
	}
}

func TestCommandMenuRegistrationCanRetry(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.tg.SetCommandError(errors.New("temporary failure"))
	if err := h.b.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.tg.Commands()) != 0 {
		t.Fatal("failed registration was recorded as successful")
	}
	h.tg.SetCommandError(nil)
	h.b.registerCommands(context.Background())
	if len(h.tg.Commands()) != 10 {
		t.Fatal("command menu was not registered after recovery")
	}
}

func TestArchiveButtonsChooseSessionAndCheckOwner(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	first := sessionIDIn(h.text(testkit.TopicUpdate(1, aliceChat, 17, aliceID, "/new first")))
	second := sessionIDIn(h.text(testkit.TopicUpdate(2, aliceChat, 18, aliceID, "/new second")))
	if first == "" || second == "" || first == second {
		t.Fatalf("session ids = %q, %q", first, second)
	}
	h.text(testkit.TopicUpdate(3, aliceChat, 17, aliceID, "/archive"))
	sent := h.tg.Sent()
	panel := sent[len(sent)-1]
	if panel.ThreadID != 17 || panel.Keyboard == nil || len(panel.Keyboard.InlineKeyboard) != 2 ||
		panel.Keyboard.InlineKeyboard[0][0].CallbackData != "archive:"+second ||
		panel.Keyboard.InlineKeyboard[1][0].CallbackData != "archive:"+first ||
		!strings.Contains(panel.Text, "* "+first+"  first") ||
		!strings.Contains(panel.Text, "  "+second+"  second") {
		t.Fatalf("archive panel = %+v", panel)
	}
	if got, _ := h.st.GetSession(context.Background(), first, aliceID); got.Archived {
		t.Fatal("showing the archive choices already archived a session")
	}
	forged := buttonUpdate(4, bobChat, 0, bobID, "archive:"+second)
	h.text(forged)
	if got, _ := h.st.GetSession(context.Background(), second, aliceID); got.Archived {
		t.Fatal("another user archived Alice's session")
	}
	choice := buttonUpdate(5, aliceChat, 17, aliceID, "archive:"+first)
	choice.CallbackQuery.Message.MessageID = panel.MessageID
	h.text(choice)
	if got, _ := h.st.GetSession(context.Background(), first, aliceID); !got.Archived {
		t.Fatal("chosen session was not archived")
	}
	if got, _ := h.st.GetSession(context.Background(), second, aliceID); got.Archived {
		t.Fatal("other session was archived")
	}
	updated := h.tg.Sent()
	if len(updated) != len(sent)+1 {
		t.Fatalf("archive callback sent unexpected messages: %+v", updated)
	}
	archivedPanel := updated[len(sent)-1]
	if !strings.Contains(archivedPanel.Text, "Archived "+first) ||
		archivedPanel.Keyboard == nil || len(archivedPanel.Keyboard.InlineKeyboard) != 0 {
		t.Fatalf("archive panel was not completed in place: %+v", archivedPanel)
	}
	if got := selected(t, h, aliceChat, 18, aliceID); got != second {
		t.Fatalf("archiving a session changed another topic's selection to %q", got)
	}
	if got := h.text(msg(6, bobChat, bobID, "/archive")); !strings.Contains(got, "no sessions to archive") {
		t.Errorf("empty archive list replied %q", got)
	}
}

func TestSessionButtonsSwitchWithinTopicAndRejectOtherOwner(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	first := sessionIDIn(h.text(testkit.TopicUpdate(1, aliceChat, 17, aliceID, "/new first")))
	second := sessionIDIn(h.text(testkit.TopicUpdate(2, aliceChat, 17, aliceID, "/new second")))
	if first == "" || second == "" || first == second {
		t.Fatalf("session ids = %q, %q", first, second)
	}
	h.text(testkit.TopicUpdate(3, aliceChat, 17, aliceID, "/sessions"))
	sent := h.tg.Sent()
	panel := sent[len(sent)-1]
	if panel.ThreadID != 17 || panel.Keyboard == nil || len(panel.Keyboard.InlineKeyboard) != 2 ||
		panel.Keyboard.InlineKeyboard[0][0].CallbackData != "use:"+second {
		t.Fatalf("session panel = %+v", panel)
	}
	button := buttonUpdate(4, aliceChat, 17, aliceID, "use:"+first)
	button.CallbackQuery.Message.MessageID = panel.MessageID
	h.text(button)
	if got := selected(t, h, aliceChat, 17, aliceID); got != first {
		t.Fatalf("button selected %s, want %s", got, first)
	}
	updated := h.tg.Sent()
	if len(updated) != len(sent) || !strings.Contains(updated[len(updated)-1].Text, "Selected "+first) ||
		!strings.HasPrefix(updated[len(updated)-1].Keyboard.InlineKeyboard[1][0].Text, "✓ ") {
		t.Fatalf("button did not update the original session panel: %+v", updated[len(updated)-1])
	}
	same := buttonUpdate(5, aliceChat, 17, aliceID, "use:"+first)
	same.CallbackQuery.Message.MessageID = panel.MessageID
	h.text(same)
	if len(h.tg.Sent()) != len(sent) {
		t.Fatal("repeated session button sent a confirmation")
	}
	if answers := h.tg.CallbackAnswers(); len(answers) != 2 || answers[0].ID != "cb-4" || answers[1].ID != "cb-5" {
		t.Fatalf("button acknowledgements = %+v", answers)
	}
	// A callback payload can be forged; ownership is still checked by /use.
	h.text(buttonUpdate(6, bobChat, 0, bobID, "use:"+first))
	if _, err := h.st.GetSelection(context.Background(), bobChat, 0, bobID); err == nil {
		t.Fatal("another owner selected Alice's session through a button")
	}
	if got := h.tg.LastText(); !strings.Contains(got, "could not find that session") {
		t.Fatalf("foreign session button replied %q", got)
	}
}

func TestSessionButtonFallsBackWhenOriginalMessageCannotBeEdited(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	first := sessionIDIn(h.text(msg(1, aliceChat, aliceID, "/new first")))
	h.text(msg(2, aliceChat, aliceID, "/new second"))
	h.text(msg(3, aliceChat, aliceID, "/sessions"))
	panel := h.tg.Sent()[len(h.tg.Sent())-1]
	h.tg.SetEditError(errors.New("message can't be edited"))
	button := buttonUpdate(4, aliceChat, 0, aliceID, "use:"+first)
	button.CallbackQuery.Message.MessageID = panel.MessageID
	h.text(button)
	if got := selected(t, h, aliceChat, 0, aliceID); got != first {
		t.Fatalf("selected = %s, want %s", got, first)
	}
	if len(h.tg.Sent()) != 4 || !strings.Contains(h.tg.LastText(), "Selected "+first) {
		t.Fatalf("no fallback confirmation: %+v", h.tg.Sent())
	}
}

func TestModelButtonsRespectTopicAndDefaultScope(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.text(testkit.TopicUpdate(1, aliceChat, 17, aliceID, "/model"))
	panel := h.tg.Sent()[0]
	if panel.Keyboard == nil || len(panel.Keyboard.InlineKeyboard) != 2 ||
		panel.Keyboard.InlineKeyboard[0][1].CallbackData != "model:here:luna" {
		t.Fatalf("model panel = %+v", panel)
	}
	choice := buttonUpdate(2, aliceChat, 17, aliceID, "model:here:luna")
	choice.CallbackQuery.Message.MessageID = panel.MessageID
	h.text(choice)
	if got, _ := h.st.ModelSetting(context.Background(), aliceID, aliceChat, 17); got != "gpt-6-luna" {
		t.Fatalf("topic model = %q", got)
	}
	updated := h.tg.Sent()
	if len(updated) != 1 || updated[0].Keyboard == nil ||
		updated[0].Keyboard.InlineKeyboard[0][1].Text != "✓ Here: Luna" ||
		!strings.Contains(updated[0].Text, "Model here: gpt-6-luna") {
		t.Fatalf("model panel was not updated in place: %+v", updated)
	}
	defaultChoice := buttonUpdate(3, aliceChat, 17, aliceID, "model:default:sol")
	defaultChoice.CallbackQuery.Message.MessageID = panel.MessageID
	h.text(defaultChoice)
	if got, _ := h.st.ModelSetting(context.Background(), aliceID, 0, 0); got != "gpt-6-sol" {
		t.Fatalf("default model = %q", got)
	}
	if got, _ := h.st.ModelSetting(context.Background(), aliceID, aliceChat, 17); got != "gpt-6-luna" {
		t.Fatalf("default button overwrote topic model: %q", got)
	}
	updated = h.tg.Sent()
	if len(updated) != 1 || updated[0].Keyboard.InlineKeyboard[1][0].Text != "✓ Default: Sol" {
		t.Fatalf("default choice was not updated in place: %+v", updated)
	}
	before := len(h.tg.Sent())
	h.text(defaultChoice)
	if len(h.tg.Sent()) != before {
		t.Fatal("duplicate callback sent a second confirmation")
	}
	if len(h.tg.CallbackAnswers()) != 3 {
		t.Fatal("a callback press was left spinning")
	}
	if len(h.fake.Invocations(t)) != 0 {
		t.Fatal("model button unexpectedly started a Codex turn")
	}
}

func TestModelButtonFallsBackWhenOriginalMessageCannotBeEdited(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.text(msg(1, aliceChat, aliceID, "/model"))
	panel := h.tg.Sent()[0]
	h.tg.SetEditError(errors.New("message can't be edited"))
	choice := buttonUpdate(2, aliceChat, 0, aliceID, "model:here:luna")
	choice.CallbackQuery.Message.MessageID = panel.MessageID
	h.text(choice)
	if got, _ := h.st.ModelSetting(context.Background(), aliceID, aliceChat, 0); got != "gpt-6-luna" {
		t.Fatalf("model = %q, want gpt-6-luna", got)
	}
	if len(h.tg.Sent()) != 2 || !strings.Contains(h.tg.LastText(), "Model here: gpt-6-luna") {
		t.Fatalf("no fallback confirmation: %+v", h.tg.Sent())
	}
}

func TestInvalidAndUnauthorizedButtons(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.text(buttonUpdate(1, aliceChat, 0, aliceID, "use:../../bad"))
	if answers := h.tg.CallbackAnswers(); len(answers) != 1 || !strings.Contains(answers[0].Text, "unavailable") {
		t.Fatalf("invalid callback was not explained: %+v", answers)
	}
	if len(h.tg.Sent()) != 0 {
		t.Fatal("invalid button produced a chat message")
	}
	expired := buttonUpdate(2, aliceChat, 0, aliceID, "model:here:luna")
	expired.CallbackQuery.Message.Date = 0
	h.text(expired)
	if answers := h.tg.CallbackAnswers(); len(answers) != 2 || !strings.Contains(answers[1].Text, "unavailable") {
		t.Fatalf("expired callback was not explained: %+v", answers)
	}
	if got, _ := h.st.ModelSetting(context.Background(), aliceID, aliceChat, 0); got != "" {
		t.Fatalf("expired callback changed the model: %q", got)
	}
	h.text(buttonUpdate(3, malloryID, 0, malloryID, "model:here:sol"))
	if len(h.tg.CallbackAnswers()) != 2 {
		t.Fatal("unauthorized callback was acknowledged")
	}
	group := buttonUpdate(4, -1001, 0, aliceID, "model:here:sol")
	group.CallbackQuery.Message.Chat.Type = "supergroup"
	h.text(group)
	if len(h.tg.CallbackAnswers()) != 2 {
		t.Fatal("group callback was acknowledged")
	}
}

func TestButtonIsAcknowledgedWhileTurnIsRunning(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: slowSpec()})
	newSession(t, h, 1, aliceChat, aliceID, "slow")
	turn, err := h.b.Claim(context.Background(), msg(2, aliceChat, aliceID, "slow task"))
	if err != nil {
		t.Fatal(err)
	}
	h.b.dispatch(context.Background(), turn)
	h.fake.WaitForEmitted(t, 1, 15*time.Second)
	button, err := h.b.Claim(context.Background(), buttonUpdate(3, aliceChat, 0, aliceID, "model:here:luna"))
	if err != nil {
		t.Fatal(err)
	}
	h.b.dispatch(context.Background(), button)
	deadline := time.Now().Add(3 * time.Second)
	for len(h.tg.CallbackAnswers()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(h.tg.CallbackAnswers()) != 1 {
		t.Fatal("model button was left spinning behind the active turn")
	}
	if got, _ := h.st.ModelSetting(context.Background(), aliceID, aliceChat, 0); got != "" {
		t.Fatalf("model button overtook the active turn: %q", got)
	}
	stop, err := h.b.Claim(context.Background(), msg(4, aliceChat, aliceID, "/stop"))
	if err != nil {
		t.Fatal(err)
	}
	h.b.dispatch(context.Background(), stop)
	h.b.Wait()
	if got, _ := h.st.ModelSetting(context.Background(), aliceID, aliceChat, 0); got != "gpt-6-luna" {
		t.Fatalf("queued model button never applied: %q", got)
	}
}

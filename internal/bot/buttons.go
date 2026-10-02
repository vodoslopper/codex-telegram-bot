package bot

import (
	"context"
	"strings"

	"codex-telegram-bot/internal/sessid"
	"codex-telegram-bot/internal/store"
	"codex-telegram-bot/internal/telegram"
)

func botCommands() []telegram.BotCommand {
	return []telegram.BotCommand{
		{Command: "start", Description: "Show the bot and current session"},
		{Command: "help", Description: "Show commands and workspace"},
		{Command: "new", Description: "Start a new session"},
		{Command: "sessions", Description: "List and switch sessions"},
		{Command: "session", Description: "Show the selected session and status"},
		{Command: "rename", Description: "Rename the selected session"},
		{Command: "archive", Description: "Choose a session to archive"},
		{Command: "usage", Description: "Show context and rate limits"},
		{Command: "model", Description: "Choose the model here or by default"},
		{Command: "stop", Description: "Cancel the active turn"},
	}
}

// Button data is deliberately restricted to the same short, validated inputs
// accepted by the text commands. A client may send arbitrary callback data.
func commandFromButton(data string) *Command {
	if id, ok := strings.CutPrefix(data, "use:"); ok && sessid.Valid(id) {
		return &Command{Name: "use", Args: []string{id}}
	}
	if id, ok := strings.CutPrefix(data, "archive:"); ok && sessid.Valid(id) {
		return &Command{Name: "archive", Args: []string{id}}
	}
	if value, ok := strings.CutPrefix(data, "model:here:"); ok && modelButtonValue(value) {
		return &Command{Name: "model", Args: []string{value}}
	}
	if value, ok := strings.CutPrefix(data, "model:default:"); ok && modelButtonValue(value) {
		return &Command{Name: "model", Args: []string{"default", value}}
	}
	return nil
}

func modelButtonValue(value string) bool {
	return value == "sol" || value == "luna" || value == "reset"
}

func (b *Bot) modelKeyboard(ctx context.Context, scope Scope) (*telegram.InlineKeyboard, error) {
	local, err := b.st.ModelSetting(ctx, scope.UserID, scope.ChatID, scope.ThreadID)
	if err != nil {
		return nil, err
	}
	userDefault, err := b.st.ModelSetting(ctx, scope.UserID, 0, 0)
	if err != nil {
		return nil, err
	}
	mark := func(label string, selected bool) string {
		if selected {
			return "✓ " + label
		}
		return label
	}
	return &telegram.InlineKeyboard{InlineKeyboard: [][]telegram.InlineButton{
		{
			{Text: mark("Here: Sol", local == "gpt-6-sol"), CallbackData: "model:here:sol"},
			{Text: mark("Here: Luna", local == "gpt-6-luna"), CallbackData: "model:here:luna"},
			{Text: mark("Here: inherit", local == ""), CallbackData: "model:here:reset"},
		},
		{
			{Text: mark("Default: Sol", userDefault == "gpt-6-sol"), CallbackData: "model:default:sol"},
			{Text: mark("Default: Luna", userDefault == "gpt-6-luna"), CallbackData: "model:default:luna"},
			{Text: mark("Default: reset", userDefault == ""), CallbackData: "model:default:reset"},
		},
	}}, nil
}

const maxSessionButtons = 8

func sessionsKeyboard(sessions []store.Session, selected string) *telegram.InlineKeyboard {
	rows := make([][]telegram.InlineButton, 0, min(len(sessions), maxSessionButtons))
	for _, session := range sessions[:min(len(sessions), maxSessionButtons)] {
		name := strings.Join(strings.Fields(session.Name), " ")
		if name == "" {
			name = "unnamed"
		}
		label := session.ID + " · " + truncateRunes(name, 28)
		if session.ID == selected {
			label = "✓ " + label
		}
		rows = append(rows, []telegram.InlineButton{{Text: label, CallbackData: "use:" + session.ID}})
	}
	return &telegram.InlineKeyboard{InlineKeyboard: rows}
}

func archiveKeyboard(sessions []store.Session) *telegram.InlineKeyboard {
	rows := make([][]telegram.InlineButton, 0, min(len(sessions), maxSessionButtons))
	for _, session := range sessions[:min(len(sessions), maxSessionButtons)] {
		name := strings.Join(strings.Fields(session.Name), " ")
		if name == "" {
			name = "unnamed"
		}
		rows = append(rows, []telegram.InlineButton{{
			Text:         "Archive " + session.ID + " · " + truncateRunes(name, 28),
			CallbackData: "archive:" + session.ID,
		}})
	}
	return &telegram.InlineKeyboard{InlineKeyboard: rows}
}

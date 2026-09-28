package bot

import (
	"context"
	"fmt"
	"strings"
)

func modelName(value string) string {
	switch strings.ToLower(value) {
	case "luna", "gpt-6-luna":
		return "gpt-6-luna"
	case "sol", "gpt-6-sol":
		return "gpt-6-sol"
	default:
		return ""
	}
}

func (b *Bot) effectiveModel(ctx context.Context, scope Scope) (string, error) {
	choice, err := b.st.ModelSetting(ctx, scope.UserID, scope.ChatID, scope.ThreadID)
	if err != nil {
		return "", err
	}
	if choice != "" {
		return choice, nil
	}
	choice, err = b.st.ModelSetting(ctx, scope.UserID, 0, 0)
	if err != nil {
		return "", err
	}
	if choice != "" {
		return choice, nil
	}
	return b.cfg.Model, nil
}

func (b *Bot) cmdModel(ctx context.Context, p *Prepared, cmd *Command) error {
	usage := "Usage: /model [luna|sol|reset] or /model default [luna|sol|reset]."
	args := cmd.Args
	if len(args) == 0 {
		current, err := b.effectiveModel(ctx, p.Scope)
		if err != nil {
			return err
		}
		local, err := b.st.ModelSetting(ctx, p.Scope.UserID, p.Scope.ChatID, p.Scope.ThreadID)
		if err != nil {
			return err
		}
		source := "your default"
		if local != "" {
			source = "this chat or topic"
		} else if userDefault, err := b.st.ModelSetting(ctx, p.Scope.UserID, 0, 0); err == nil && userDefault == "" {
			source = "bot setting"
		}
		keyboard, err := b.modelKeyboard(ctx, p.Scope)
		if err != nil {
			return err
		}
		b.sendBestWithKeyboard(ctx, p.Scope, fmt.Sprintf("Model here: %s (%s).\n%s", current, source, usage), keyboard)
		return nil
	}
	chatID, threadID := p.Scope.ChatID, p.Scope.ThreadID
	target := "this chat or topic"
	if strings.EqualFold(args[0], "default") {
		if len(args) != 2 {
			b.sendBest(ctx, p.Scope, usage)
			return nil
		}
		chatID, threadID = 0, 0
		target = "your default"
		args = args[1:]
	}
	if len(args) != 1 {
		b.sendBest(ctx, p.Scope, usage)
		return nil
	}
	value := modelName(args[0])
	if strings.EqualFold(args[0], "reset") {
		value = ""
	} else if value == "" {
		b.sendBest(ctx, p.Scope, usage)
		return nil
	}
	if err := b.st.SetModelSetting(ctx, p.Scope.UserID, chatID, threadID, value); err != nil {
		return err
	}
	current, err := b.effectiveModel(ctx, p.Scope)
	if err != nil {
		return err
	}
	keyboard, err := b.modelKeyboard(ctx, p.Scope)
	if err != nil {
		return err
	}
	b.sendBestWithKeyboard(ctx, p.Scope, fmt.Sprintf("Model setting for %s updated. Model here: %s. New turns use this choice, including resumed sessions.", target, current), keyboard)
	return nil
}

package bot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var fileLine = regexp.MustCompile(`^\[\[telegram-file:(/[^\r\n]+)\]\]$`)

const fileHandoffInstruction = "\n\nIf you create a file that the user should receive in Telegram, include a separate line in your final answer for each file in this exact form: [[telegram-file:/absolute/path/to/file]]. Create the file inside the current workspace. Do not claim a file is attached unless you include this line."

// deliverReply sends files explicitly named in the final answer, then its text.
// The marker stays in the stored reply so a failed delivery can be retried.
func (b *Bot) deliverReply(ctx context.Context, scope Scope, reply string, startedAt time.Time) error {
	var body []string
	var paths []string
	for _, line := range strings.Split(reply, "\n") {
		if m := fileLine.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			paths = append(paths, m[1])
		} else {
			body = append(body, line)
		}
	}
	if len(paths) > 5 {
		return fmt.Errorf("reply names %d files; limit is 5", len(paths))
	}
	for _, path := range paths {
		if err := b.validateOutboundFile(path, startedAt); err != nil {
			return err
		}
		if _, err := b.tg.SendDocument(ctx, scope.ChatID, scope.ThreadID, path); err != nil {
			return fmt.Errorf("send document: %w", err)
		}
	}
	return b.send(ctx, scope, strings.Join(body, "\n"))
}

func (b *Bot) validateOutboundFile(path string, startedAt time.Time) error {
	root, err := filepath.EvalSymlinks(b.cfg.Workspace)
	if err != nil {
		return fmt.Errorf("resolve workspace: %w", err)
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve attachment: %w", err)
	}
	rel, err := filepath.Rel(root, realPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator)) {
		return fmt.Errorf("attachment is outside the allowed workspace")
	}
	info, err := os.Stat(realPath)
	if err != nil {
		return fmt.Errorf("stat attachment: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > 50<<20 {
		return fmt.Errorf("attachment must be a nonempty regular file no larger than 50 MB")
	}
	if info.ModTime().Before(startedAt) {
		return fmt.Errorf("attachment predates this turn")
	}
	return nil
}

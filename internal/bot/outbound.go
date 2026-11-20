package bot

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var fileLine = regexp.MustCompile(`^\[\[telegram-file:(/[^\r\n]+)\]\]$`)

const fileHandoffInstruction = "\n\nIf you create a file that the user should receive in Telegram, include a separate line in your final answer for each file in this exact form: [[telegram-file:/absolute/path/to/file]]. Create the file inside the current workspace. Do not claim a file is attached unless you include this line. For a resend, include the marker for the unchanged file previously sent in this session.\n\nRespect explicit source and date limits when collecting evidence. When the user names a collection under a parent directory, try the named child directly (parent/collection). Scope every recursive listing, search and read to that child. If the location is uncertain, list only the immediate child directories to locate it. Never enumerate files in sibling collections. Attribute events to their named projects and link separate events only when the supplied evidence establishes that relationship."

// deliverReply sends files explicitly named in the final answer, then its text.
// The marker stays in the stored reply so a failed delivery can be retried.
func (b *Bot) deliverReply(ctx context.Context, scope Scope, sessionID, reply string, startedAt time.Time) error {
	var body []string
	var paths []string
	for _, line := range strings.Split(reply, "\n") {
		if m := fileLine.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			paths = append(paths, m[1])
		} else if isRelatedMediaLine(strings.TrimSpace(line)) {
			continue
		} else {
			body = append(body, line)
		}
	}
	omitted := 0
	if len(paths) > 5 {
		omitted = len(paths) - 5
		paths = paths[:5]
	}
	for _, path := range paths {
		data, modified, err := b.readOutboundFile(path)
		if err != nil {
			omitted++
			b.log.Warn("generated file could not be attached", "error", err.Error())
			continue
		}
		digest := sha256.Sum256(data)
		if modified.Before(startedAt) {
			sent, err := b.st.WasDocumentSent(ctx, sessionID, path, digest[:])
			if err != nil {
				return fmt.Errorf("check earlier upload: %w", err)
			}
			if !sent {
				omitted++
				b.log.Warn("generated file could not be attached", "error", "attachment predates this turn")
				continue
			}
		}
		if _, err := b.tg.SendDocument(ctx, scope.ChatID, scope.ThreadID, filepath.Base(path), data); err != nil {
			return fmt.Errorf("send document: %w", err)
		}
		if err := b.st.RecordSentDocument(ctx, sessionID, path, digest[:]); err != nil {
			return fmt.Errorf("record document upload: %w", err)
		}
	}
	text := strings.TrimSpace(strings.Join(body, "\n"))
	if omitted > 0 {
		unit := "file"
		if omitted != 1 {
			unit = "files"
		}
		note := fmt.Sprintf("I could not attach %d generated %s: missing or beyond the file limits. Check the bot log for details.", omitted, unit)
		if text != "" {
			text += "\n\n" + note
		} else {
			text = note
		}
	}
	return b.send(ctx, scope, text)
}

func isRelatedMediaLine(line string) bool {
	return strings.HasPrefix(line, "[[telegram-media-related:") && strings.HasSuffix(line, "]]")
}

func (b *Bot) readOutboundFile(path string) ([]byte, time.Time, error) {
	root, err := filepath.EvalSymlinks(b.cfg.Workspace)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("resolve workspace: %w", err)
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("resolve attachment: %w", err)
	}
	rel, err := filepath.Rel(root, realPath)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".git" || strings.HasPrefix(rel, ".git"+string(filepath.Separator)) {
		return nil, time.Time{}, fmt.Errorf("attachment is outside the allowed workspace")
	}
	info, err := os.Stat(realPath)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("stat attachment: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > 50<<20 {
		return nil, time.Time{}, fmt.Errorf("attachment must be a nonempty regular file no larger than 50 MB")
	}
	file, err := os.OpenInRoot(root, rel)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(opened, info) {
		return nil, time.Time{}, fmt.Errorf("attachment changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, (50<<20)+1))
	if err != nil || int64(len(data)) != opened.Size() || len(data) == 0 || len(data) > 50<<20 {
		return nil, time.Time{}, fmt.Errorf("could not read a complete attachment within the file limit")
	}
	return data, opened.ModTime(), nil
}

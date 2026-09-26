package bot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"codex-telegram-bot/internal/store"
	"codex-telegram-bot/internal/telegram"
)

// plannedMediaPath chooses a private, unpredictable location without creating
// files yet. The workspace lock is acquired before anything is written there.
func plannedMediaPath(workspace, ext string) (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("allocate media path: %w", err)
	}
	return filepath.Join(workspace, ".codex-telegram-media-"+hex.EncodeToString(nonce[:]), "attachment"+ext), nil
}

type mediaAttachment struct {
	kind   string
	fileID string
	size   int64
	ext    string
	image  bool
}

// attachmentOf chooses the largest resolution of a photo and one primary file
// for other supported Telegram media messages.
func attachmentOf(m *telegram.Message) *mediaAttachment {
	if m == nil {
		return nil
	}
	if len(m.Photo) > 0 {
		best := m.Photo[0]
		for _, photo := range m.Photo[1:] {
			if int64(photo.Width)*int64(photo.Height) > int64(best.Width)*int64(best.Height) {
				best = photo
			}
		}
		return &mediaAttachment{kind: "photo", fileID: best.FileID, size: best.FileSize, ext: ".jpg", image: true}
	}
	var file *telegram.MediaFile
	kind := ""
	switch {
	case m.Document != nil:
		file, kind = m.Document, "document"
	case m.Audio != nil:
		file, kind = m.Audio, "audio"
	case m.Voice != nil:
		file, kind = m.Voice, "voice"
	case m.Video != nil:
		file, kind = m.Video, "video"
	case m.VideoNote != nil:
		file, kind = m.VideoNote, "video"
	default:
		return nil
	}
	ext := strings.ToLower(filepath.Ext(file.FileName))
	if len(ext) > 12 || len(ext) < 2 {
		ext = ""
	}
	for _, r := range ext {
		if r != '.' && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			ext = ""
			break
		}
	}
	if ext == "" {
		switch kind {
		case "audio":
			ext = ".mp3"
		case "voice":
			ext = ".ogg"
		case "video":
			ext = ".mp4"
		default:
			ext = ".bin"
		}
	}
	image := kind == "document" && (file.MimeType == "image/jpeg" || file.MimeType == "image/png" || file.MimeType == "image/webp")
	if image {
		switch file.MimeType {
		case "image/jpeg":
			ext = ".jpg"
		case "image/png":
			ext = ".png"
		case "image/webp":
			ext = ".webp"
		}
	}
	return &mediaAttachment{kind: kind, fileID: file.FileID, size: file.FileSize, ext: ext, image: image}
}

// stageMedia writes one downloaded attachment into the workspace while its
// lock is held. The caller keeps it for the session or calls cleanup on failure.
func (b *Bot) stageMedia(ctx context.Context, media *mediaAttachment, path string) (func(), error) {
	if media == nil {
		return func() {}, nil
	}
	if media.size > telegram.MaxDownloadBytes {
		return nil, telegram.ErrFileTooLarge
	}
	data, err := b.tg.DownloadFile(ctx, media.fileID)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create media directory: %w", err)
	}
	cleanup := func() {
		if err := os.RemoveAll(dir); err != nil {
			b.log.Warn("could not remove staged media", "error", err.Error())
		}
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		cleanup()
		return nil, fmt.Errorf("write media attachment: %w", err)
	}
	return cleanup, nil
}

func mediaPrompt(caption string, media *mediaAttachment, path string) string {
	caption = strings.TrimSpace(caption)
	if caption == "" {
		caption = "Please inspect the attached " + media.kind + " and describe what you can determine."
	}
	return caption + "\n\nAttached " + media.kind + " file: " + path
}

func mediaFailure(err error) string {
	if errors.Is(err, context.Canceled) {
		return "This turn was cancelled before Codex started."
	}
	if errors.Is(err, telegram.ErrFileTooLarge) {
		return "That file is too large to download through the Telegram Bot API (20 MB limit)."
	}
	return "I could not download or prepare that attachment. Please retry or send a smaller file."
}

// retainedMediaPrompt gives a resumed Codex turn the paths it may still use,
// and asks that same turn to judge which files its current message concerns.
func retainedMediaPrompt(media []store.RetainedMedia) string {
	if len(media) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nRetained Telegram files from earlier turns in this session:\n")
	for _, m := range media {
		fmt.Fprintf(&b, "- ID %d (%s): %s\n", m.ID, m.Kind, m.Path)
	}
	b.WriteString("For each listed file relevant to the user's current message, add a separate line [[telegram-media-related:ID]] to your final answer, replacing ID with that file's number. Judge relevance from the message and conversation. Do not add the line for unrelated files. These lines are for the bot and are removed before sending the answer.")
	return b.String()
}

func relatedMediaIDs(reply string) map[int64]bool {
	ids := make(map[int64]bool)
	for _, line := range strings.Split(reply, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "[[telegram-media-related:") || !strings.HasSuffix(line, "]]") {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(line, "[[telegram-media-related:"), "]]"), 10, 64)
		if err == nil && n > 0 {
			ids[n] = true
		}
	}
	return ids
}

func removeMediaDirectory(workspace, path string) error {
	dir := filepath.Dir(path)
	root, err := filepath.Abs(workspace)
	if err != nil {
		return err
	}
	if filepath.Dir(dir) != root || !strings.HasPrefix(filepath.Base(dir), ".codex-telegram-media-") || !strings.HasPrefix(filepath.Base(path), "attachment.") {
		return fmt.Errorf("refusing to remove an unexpected media path")
	}
	return os.RemoveAll(dir)
}

func (b *Bot) advanceRetainedMedia(ctx context.Context, workspace string, ownerUserID int64, media []store.RetainedMedia, related map[int64]bool) {
	for _, m := range media {
		if related[m.ID] {
			continue
		}
		expired, err := b.st.IncrementMediaUnrelated(ctx, m.ID, ownerUserID)
		if err != nil {
			b.log.Warn("could not count unrelated turn for media", "media_id", m.ID, "error", err.Error())
			continue
		}
		if expired {
			if err := removeMediaDirectory(workspace, m.Path); err != nil {
				b.log.Warn("could not remove expired media", "media_id", m.ID, "error", err.Error())
				continue
			}
			if err := b.st.DeleteRetainedMedia(ctx, m.ID, ownerUserID); err != nil {
				b.log.Warn("could not forget expired media", "media_id", m.ID, "error", err.Error())
			}
		}
	}
}

package bot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
// lock is held. The caller removes the private directory after the turn.
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

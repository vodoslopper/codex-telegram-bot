package bot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-telegram-bot/internal/telegram"
	"codex-telegram-bot/internal/testkit"
)

func TestPhotoUsesLargestResolutionAndImageFlag(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("seen", "seen")})
	h.tg.SetFile("large", []byte("image bytes"))
	u := msg(1, aliceChat, aliceID, "")
	u.Message.Caption = "What is in this picture?"
	u.Message.Photo = []telegram.PhotoSize{
		{FileID: "small", Width: 100, Height: 100},
		{FileID: "large", Width: 800, Height: 600},
	}
	if got := h.text(u); !strings.Contains(got, "seen") {
		t.Fatal(got)
	}
	argv := h.fake.Argv(t, 1)
	var imagePath string
	for i := range argv {
		if argv[i] == "-i" && i+1 < len(argv) {
			imagePath = argv[i+1]
		}
	}
	if imagePath == "" || !strings.HasSuffix(imagePath, ".jpg") {
		t.Fatalf("image path missing: %q", argv)
	}
	if !strings.Contains(argv[len(argv)-1], "What is in this picture?") {
		t.Errorf("caption missing: %q", argv)
	}
	if _, err := os.Stat(filepath.Dir(imagePath)); !os.IsNotExist(err) {
		t.Errorf("staged media was not removed: %v", err)
	}
}

func TestPhotoAttachesWhenSessionResumes(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("first", "second")})
	h.text(msg(1, aliceChat, aliceID, "start a thread"))
	h.tg.SetFile("photo", []byte("image bytes"))
	u := msg(2, aliceChat, aliceID, "")
	u.Message.Photo = []telegram.PhotoSize{{FileID: "photo", Width: 320, Height: 240}}
	if got := h.text(u); got != "second" {
		t.Fatalf("resumed reply = %q", got)
	}
	argv := h.fake.Argv(t, 2)
	if len(argv) < 3 || argv[0] != "exec" || argv[1] != "resume" {
		t.Fatalf("Codex did not resume: %q", argv)
	}
	if !strings.Contains(strings.Join(argv, " "), " -i ") {
		t.Errorf("resumed turn lacks image flag: %q", argv)
	}
}

func TestFileMediaIsAvailableAsLocalAttachment(t *testing.T) {
	cases := []struct {
		name  string
		set   func(*telegram.Message)
		image bool
	}{
		{"document", func(m *telegram.Message) { m.Document = &telegram.MediaFile{FileID: "file", FileName: "report.pdf"} }, false},
		{"image document", func(m *telegram.Message) {
			m.Document = &telegram.MediaFile{FileID: "file", FileName: "wrong.txt", MimeType: "image/png"}
		}, true},
		{"audio", func(m *telegram.Message) { m.Audio = &telegram.MediaFile{FileID: "file", FileName: "song.mp3"} }, false},
		{"voice", func(m *telegram.Message) { m.Voice = &telegram.MediaFile{FileID: "file"} }, false},
		{"video", func(m *telegram.Message) { m.Video = &telegram.MediaFile{FileID: "file", FileName: "clip.mp4"} }, false},
		{"video note", func(m *telegram.Message) { m.VideoNote = &telegram.MediaFile{FileID: "file"} }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, harnessOpts{spec: successSpec("seen", "seen")})
			h.tg.SetFile("file", []byte("file contents"))
			u := msg(1, aliceChat, aliceID, "")
			u.Message.Caption = "Inspect this"
			tc.set(u.Message)
			if got := h.text(u); !strings.Contains(got, "seen") {
				t.Fatal(got)
			}
			argv := h.fake.Argv(t, 1)
			prompt := argv[len(argv)-1]
			if !strings.Contains(prompt, "Inspect this") || !strings.Contains(prompt, "Attached ") {
				t.Errorf("prompt lacks caption or attachment: %q", prompt)
			}
			hasImage := false
			for _, arg := range argv {
				if arg == "-i" {
					hasImage = true
				}
			}
			if hasImage != tc.image {
				t.Errorf("image flag = %v, want %v: %q", hasImage, tc.image, argv)
			}
			path := strings.TrimSpace(prompt[strings.LastIndex(prompt, "file: ")+len("file: "):])
			if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
				t.Errorf("staged media was not removed: %v", err)
			}
		})
	}
}

func TestMediaDownloadLimitDoesNotRunCodex(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("seen", "seen")})
	u := msg(1, aliceChat, aliceID, "")
	u.Message.Document = &telegram.MediaFile{FileID: "huge", FileSize: telegram.MaxDownloadBytes + 1}
	if got := h.text(u); !strings.Contains(got, "too large") {
		t.Fatal(got)
	}
	if n := len(h.fake.Invocations(t)); n != 0 {
		t.Fatalf("Codex ran %d times", n)
	}
	sessions := h.sessions(t, aliceID, false)
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d, want 1", len(sessions))
	}
	if turn := h.lastTurn(t, sessions[0].ID); turn.Status != "failed" {
		t.Fatalf("oversized media turn status = %q", turn.Status)
	}
}

func TestStageMediaWritesBytesAndCleansUp(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	h.tg.SetFile("file", []byte("hello"))
	path, err := plannedMediaPath(h.cfg.Workspace, ".txt")
	if err != nil {
		t.Fatal(err)
	}
	cleanup, err := h.b.stageMedia(context.Background(), &mediaAttachment{kind: "document", fileID: "file", ext: ".txt"}, path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "hello" {
		t.Fatalf("staged bytes = %q, error %v", data, err)
	}
	cleanup()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("media was not removed: %v", err)
	}
}

func TestMediaIsIgnoredForUnauthorizedSender(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("seen", "seen")})
	u := testkit.MessageUpdate(1, malloryID, malloryID, "")
	u.Message.Photo = []telegram.PhotoSize{{FileID: "secret", Width: 100, Height: 100}}
	h.say(u)
	if len(h.tg.Sent()) != 0 || len(h.fake.Invocations(t)) != 0 {
		t.Fatal("unauthorized media was handled")
	}
}

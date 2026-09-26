package bot

import (
	"context"
	"fmt"
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
	if _, err := os.Stat(imagePath); err != nil {
		t.Errorf("staged media was not retained: %v", err)
	}
}

func setResumeReply(t *testing.T, h *harness, reply string) {
	t.Helper()
	path := filepath.Join(filepath.Dir(h.fake.Path), "resume.txt")
	if err := os.WriteFile(path, []byte(testkit.Events(threadOne, reply)), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestMediaRetainedUntilThreeUnrelatedSessionTurns(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("photo analyzed", "unused")})
	h.tg.SetFile("photo", []byte("image bytes"))
	u := msg(1, aliceChat, aliceID, "")
	u.Message.Photo = []telegram.PhotoSize{{FileID: "photo", Width: 320, Height: 240}}
	h.text(u)
	sessions := h.sessions(t, aliceID, false)
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d", len(sessions))
	}
	sessionID := sessions[0].ID
	media, err := h.st.RetainedMedia(context.Background(), sessionID, aliceID)
	if err != nil || len(media) != 1 {
		t.Fatalf("retained media = %+v, %v", media, err)
	}
	path, id := media[0].Path, media[0].ID
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("media disappeared after its turn: %v", err)
	}

	setResumeReply(t, h, fmt.Sprintf("It is blue.\n[[telegram-media-related:%d]]", id))
	if got := h.text(msg(2, aliceChat, aliceID, "What color is it?")); got != "It is blue." {
		t.Errorf("related reply = %q", got)
	}
	argv := h.fake.Argv(t, 2)
	if !strings.Contains(argv[len(argv)-1], path) {
		t.Errorf("follow-up lacks retained image path: %q", argv)
	}
	media, _ = h.st.RetainedMedia(context.Background(), sessionID, aliceID)
	if len(media) != 1 || media[0].UnrelatedTurns != 0 {
		t.Fatalf("related turn counted as unrelated: %+v", media)
	}

	// Three turns in another session do not age this attachment.
	h.text(msg(3, aliceChat, aliceID, "/new other"))
	setResumeReply(t, h, "unrelated")
	for i := int64(4); i <= 6; i++ {
		h.text(msg(i, aliceChat, aliceID, "Other session"))
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("other session removed media: %v", err)
	}
	h.text(msg(7, aliceChat, aliceID, "/use "+sessionID))
	for i := int64(8); i <= 10; i++ {
		h.text(msg(i, aliceChat, aliceID, "Unrelated subject"))
		_, err := os.Stat(path)
		if i < 10 && err != nil {
			t.Fatalf("media removed after only %d unrelated turns: %v", i-7, err)
		}
		if i == 10 && !os.IsNotExist(err) {
			t.Fatalf("media remains after third unrelated turn: %v", err)
		}
	}
	media, _ = h.st.RetainedMedia(context.Background(), sessionID, aliceID)
	if len(media) != 0 {
		t.Fatalf("expired media is still recorded: %+v", media)
	}
}

func TestArchiveRemovesRetainedMedia(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("seen", "seen")})
	h.tg.SetFile("photo", []byte("image bytes"))
	u := msg(1, aliceChat, aliceID, "")
	u.Message.Photo = []telegram.PhotoSize{{FileID: "photo", Width: 320, Height: 240}}
	h.text(u)
	sessionID := h.sessions(t, aliceID, false)[0].ID
	media, err := h.st.RetainedMedia(context.Background(), sessionID, aliceID)
	if err != nil || len(media) != 1 {
		t.Fatalf("retained = %+v, %v", media, err)
	}
	path := media[0].Path
	if got := h.text(msg(2, aliceChat, aliceID, "/archive "+sessionID)); !strings.Contains(got, "Archived") {
		t.Fatalf("archive reply = %q", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("archive left media: %v", err)
	}
	media, err = h.st.RetainedMedia(context.Background(), sessionID, aliceID)
	if err != nil || len(media) != 0 {
		t.Fatalf("archive left media rows: %+v, %v", media, err)
	}
	// A new attachment sent to a still-selected archived session is temporary.
	h.tg.SetFile("another", []byte("another image"))
	u = msg(3, aliceChat, aliceID, "")
	u.Message.Photo = []telegram.PhotoSize{{FileID: "another", Width: 320, Height: 240}}
	h.text(u)
	media, err = h.st.RetainedMedia(context.Background(), sessionID, aliceID)
	if err != nil || len(media) != 0 {
		t.Fatalf("archived session retained new media: %+v, %v", media, err)
	}
}

func TestRetainedMediaSurvivesRestart(t *testing.T) {
	h := newHarness(t, harnessOpts{spec: successSpec("seen", "seen")})
	h.tg.SetFile("photo", []byte("image bytes"))
	u := msg(1, aliceChat, aliceID, "")
	u.Message.Photo = []telegram.PhotoSize{{FileID: "photo", Width: 320, Height: 240}}
	h.text(u)
	sessionID := h.sessions(t, aliceID, false)[0].ID
	media, err := h.st.RetainedMedia(context.Background(), sessionID, aliceID)
	if err != nil || len(media) != 1 {
		t.Fatalf("retained before restart = %+v, %v", media, err)
	}
	path := media[0].Path
	h.Close()
	restarted := newHarness(t, harnessOpts{ws: h.ws, state: h.state, home: h.home, spec: successSpec("seen", "unrelated")})
	restarted.text(msg(2, aliceChat, aliceID, "Different subject"))
	argv := restarted.fake.Argv(t, 1)
	if !strings.Contains(argv[len(argv)-1], path) {
		t.Errorf("resumed prompt lacks retained path: %q", argv)
	}
	media, err = restarted.st.RetainedMedia(context.Background(), sessionID, aliceID)
	if err != nil || len(media) != 1 || media[0].UnrelatedTurns != 1 {
		t.Fatalf("retained after restart = %+v, %v", media, err)
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
			line := strings.SplitN(prompt[strings.Index(prompt, "Attached "):], "\n", 2)[0]
			path := strings.SplitN(line, " file: ", 2)[1]
			if _, err := os.Stat(path); err != nil {
				t.Errorf("staged media was not retained: %v", err)
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

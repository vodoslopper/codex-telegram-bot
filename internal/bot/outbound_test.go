package bot

import (
	"bytes"
	"context"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-telegram-bot/internal/store"
	"codex-telegram-bot/internal/testkit"
)

func TestTurnDeliversGeneratedDocument(t *testing.T) {
	ws := testkit.Workspace(t)
	path := filepath.Join(ws, "transparent.png")
	reply := "Here is the PNG.\n[[telegram-file:" + path + "]]"
	h := newHarness(t, harnessOpts{ws: ws, spec: successSpec(reply, reply)})
	var fixture bytes.Buffer
	if err := png.Encode(&fixture, image.NewNRGBA(image.Rect(0, 0, 32, 24))); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, fixture.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	got := h.text(msg(1, aliceChat, aliceID, "Make a transparent PNG"))
	if strings.Contains(got, "telegram-file") || !strings.Contains(got, "Here is the PNG.") {
		t.Errorf("reply = %q", got)
	}
	sent := h.tg.Sent()
	if len(sent) < 2 || sent[len(sent)-2].Document != "transparent.png" {
		t.Fatalf("sent = %+v", sent)
	}
	img, err := png.Decode(bytes.NewReader(sent[len(sent)-2].Data))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds() != image.Rect(0, 0, 32, 24) {
		t.Fatalf("delivered PNG bounds = %v", img.Bounds())
	}
	for y := 0; y < 24; y++ {
		for x := 0; x < 32; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a != 0 {
				t.Fatalf("pixel (%d,%d) has alpha %d", x, y, a)
			}
		}
	}
	sessions := h.sessions(t, aliceID, false)
	if len(sessions) != 1 || h.lastTurn(t, sessions[0].ID).Status != store.TurnCompleted || !h.lastTurn(t, sessions[0].ID).Delivered {
		t.Fatal("file turn was not marked completed and delivered")
	}
}

// Run under the deployed unit's hardening to exercise proc and loopback setup.
// The sandbox subcommand needs neither a model login nor a Telegram token.
func TestWorkspaceWriteSandboxDocument(t *testing.T) {
	bin := os.Getenv("BOT_TEST_SANDBOX_BIN")
	if bin == "" {
		t.Skip("set BOT_TEST_SANDBOX_BIN to the installed Codex executable")
	}
	ws := testkit.Workspace(t)
	path := filepath.Join(ws, "ready.txt")
	reply := "[[telegram-file:" + path + "]]"
	h := newHarness(t, harnessOpts{ws: ws, spec: successSpec(reply, reply)})
	cmd := exec.CommandContext(t.Context(), bin, "sandbox", "-c", `sandbox_mode="workspace-write"`,
		"--", "sh", "-ec", `awk '$1 == "Name:" {print $2}' /proc/self/status; echo READY | tee ready.txt`)
	cmd.Dir = ws
	cmd.Env = append(os.Environ(), "CODEX_HOME="+h.home)
	output, err := cmd.Output()
	if err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			t.Logf("sandbox stderr: %s", e.Stderr)
		}
		t.Fatal(err)
	}
	fields := strings.Fields(string(output))
	if len(fields) != 2 || fields[1] != "READY" {
		t.Fatalf("expected process name and READY, got %q", output)
	}
	for _, m := range h.say(msg(1, aliceChat, aliceID, "Send ready.txt")) {
		if m.Document == "ready.txt" && string(m.Data) == "READY\n" {
			return
		}
	}
	t.Fatal("missing ready.txt or wrong contents")
}

func TestDeliverReplyUploadsFileAndStripsMarker(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	id := newSession(t, h, 1, aliceChat, aliceID, "files")
	h.tg.Reset()
	path := filepath.Join(h.ws, "transparent.png")
	data := []byte("transparent png bytes")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	reply := "Here is the PNG.\n[[telegram-file:" + path + "]]"
	if err := h.b.deliverReply(context.Background(), Scope{ChatID: aliceChat}, id, reply, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	sent := h.tg.Sent()
	if len(sent) != 2 || sent[0].Document != "transparent.png" || string(sent[0].Data) != string(data) || sent[1].Text != "Here is the PNG." {
		t.Fatalf("sent = %+v", sent)
	}
}

func TestDeliverReplySkipsInvalidFilesAndKeepsText(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	id := newSession(t, h, 1, aliceChat, aliceID, "files")
	h.tg.Reset()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(h.ws, "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{outside, link} {
		err := h.b.deliverReply(context.Background(), Scope{ChatID: aliceChat}, id, "Answer preserved.\n[[telegram-file:"+path+"]]", time.Now().Add(-time.Minute))
		if err != nil {
			t.Errorf("path %q: deliverReply = %v", path, err)
		}
	}
	stale := filepath.Join(h.ws, "old.txt")
	if err := os.WriteFile(stale, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.b.deliverReply(context.Background(), Scope{ChatID: aliceChat}, id, "[[telegram-file:"+stale+"]]", time.Now().Add(time.Minute)); err != nil {
		t.Errorf("stale file delivery = %v", err)
	}
	for _, sent := range h.tg.Sent() {
		if sent.Document != "" || !strings.Contains(sent.Text, "could not attach 1 generated file") {
			t.Fatalf("invalid attachment was sent or not explained: %+v", sent)
		}
	}
	if !strings.Contains(h.tg.AllText(), "Answer preserved.") {
		t.Fatal("the answer text was hidden by an invalid attachment")
	}
}

func TestResendPreviouslyUploadedFile(t *testing.T) {
	ws := testkit.Workspace(t)
	path := filepath.Join(ws, "book.fb2")
	marker := "[[telegram-file:" + path + "]]"
	h := newHarness(t, harnessOpts{ws: ws, spec: successSpec(marker, marker)})
	original := []byte("<FictionBook/>")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	h.say(msg(1, aliceChat, aliceID, "Send the book"))
	a := selected(t, h, aliceChat, 0, aliceID)
	old := time.Now().Add(-time.Hour)
	age := func() {
		t.Helper()
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	age()
	// The original Telegram upload must remain usable after a bot restart.
	h.Close()
	h = newHarness(t, harnessOpts{ws: ws, state: h.state, home: h.home, spec: successSpec(marker, marker)})
	resend := h.say(msg(2, aliceChat, aliceID, "Send it again"))
	if len(resend) != 1 || resend[0].Document != "book.fb2" || !bytes.Equal(resend[0].Data, original) {
		t.Fatalf("repeat upload: %+v", resend)
	}
	reject := func(update int64) {
		t.Helper()
		got := h.say(msg(update, aliceChat, aliceID, "Send the book"))
		if len(got) != 1 || got[0].Document != "" || !strings.Contains(got[0].Text, "could not attach") {
			t.Fatalf("unexpected delivery: %+v", got)
		}
	}
	newSession(t, h, 3, aliceChat, aliceID, "another session")
	reject(4)
	h.say(msg(5, aliceChat, aliceID, "/use "+a))
	if err := os.WriteFile(path, []byte("<ChangedBook/>"), 0600); err != nil {
		t.Fatal(err)
	}
	age()
	reject(6)
}

func TestTurnWithInvalidGeneratedFileStillDeliversAnswer(t *testing.T) {
	ws := testkit.Workspace(t)
	reply := "The report is ready.\n[[telegram-file:" + filepath.Join(ws, "missing.txt") + "]]"
	h := newHarness(t, harnessOpts{ws: ws, spec: successSpec(reply, reply)})
	got := h.text(msg(1, aliceChat, aliceID, "make a report"))
	if !strings.Contains(got, "The report is ready.") || !strings.Contains(got, "could not attach 1 generated file") {
		t.Fatalf("missing generated file hid the answer: %q", got)
	}
	sessions := h.sessions(t, aliceID, false)
	if len(sessions) != 1 || !h.lastTurn(t, sessions[0].ID).Delivered {
		t.Fatal("reply with invalid file was left pending for endless retries")
	}
}

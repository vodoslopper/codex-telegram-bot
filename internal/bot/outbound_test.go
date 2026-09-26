package bot

import (
	"context"
	"os"
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
	if err := os.WriteFile(path, []byte("png content"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := h.text(msg(1, aliceChat, aliceID, "Make a transparent PNG"))
	if strings.Contains(got, "telegram-file") || !strings.Contains(got, "Here is the PNG.") {
		t.Errorf("reply = %q", got)
	}
	sent := h.tg.Sent()
	if len(sent) < 2 || sent[len(sent)-2].Document != "transparent.png" || string(sent[len(sent)-2].Data) != "png content" {
		t.Fatalf("sent = %+v", sent)
	}
	sessions := h.sessions(t, aliceID, false)
	if len(sessions) != 1 || h.lastTurn(t, sessions[0].ID).Status != store.TurnCompleted || !h.lastTurn(t, sessions[0].ID).Delivered {
		t.Fatal("file turn was not marked completed and delivered")
	}
}

func TestDeliverReplyUploadsFileAndStripsMarker(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	path := filepath.Join(h.ws, "transparent.png")
	data := []byte("transparent png bytes")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	reply := "Here is the PNG.\n[[telegram-file:" + path + "]]"
	if err := h.b.deliverReply(context.Background(), Scope{ChatID: aliceChat}, reply, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	sent := h.tg.Sent()
	if len(sent) != 2 || sent[0].Document != "transparent.png" || string(sent[0].Data) != string(data) || sent[1].Text != "Here is the PNG." {
		t.Fatalf("sent = %+v", sent)
	}
}

func TestDeliverReplyRejectsOutsideAndStaleFiles(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(h.ws, "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{outside, link} {
		err := h.b.deliverReply(context.Background(), Scope{ChatID: aliceChat}, "[[telegram-file:"+path+"]]", time.Now().Add(-time.Minute))
		if err == nil || !strings.Contains(err.Error(), "outside") {
			t.Errorf("path %q: error = %v", path, err)
		}
	}
	stale := filepath.Join(h.ws, "old.txt")
	if err := os.WriteFile(stale, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := h.b.deliverReply(context.Background(), Scope{ChatID: aliceChat}, "[[telegram-file:"+stale+"]]", time.Now().Add(time.Minute)); err == nil || !strings.Contains(err.Error(), "predates") {
		t.Errorf("stale file error = %v", err)
	}
	if len(h.tg.Sent()) != 0 {
		t.Fatal("a rejected file was sent")
	}
}

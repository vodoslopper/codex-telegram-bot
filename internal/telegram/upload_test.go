package telegram

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSendDocumentMultipart(t *testing.T) {
	var gotChat, gotThread, gotName, gotData string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bot"+testToken+"/sendDocument" || r.Method != http.MethodPost {
			t.Errorf("request: %s %s", r.Method, r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parse form: %v", err)
		}
		gotChat, gotThread = r.FormValue("chat_id"), r.FormValue("message_thread_id")
		files := r.MultipartForm.File["document"]
		if len(files) != 1 {
			t.Errorf("files = %d", len(files))
		} else {
			gotName = files[0].Filename
			f, _ := files[0].Open()
			data, _ := io.ReadAll(f)
			gotData = string(data)
			_ = f.Close()
		}
		ok(w, `{"message_id":123,"date":1,"chat":{"id":111,"type":"private"}}`)
	}))
	defer srv.Close()
	c, err := New(testToken, srv.URL, 5*time.Second, nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "transparent.png")
	if err := os.WriteFile(path, []byte("png content"), 0o600); err != nil {
		t.Fatal(err)
	}
	sent, err := c.SendDocument(context.Background(), 111, 42, path)
	if err != nil || sent.MessageID != 123 {
		t.Fatalf("SendDocument = %+v, %v", sent, err)
	}
	if gotChat != "111" || gotThread != "42" || gotName != "transparent.png" || gotData != "png content" {
		t.Errorf("fields = %q %q %q %q", gotChat, gotThread, gotName, gotData)
	}
}

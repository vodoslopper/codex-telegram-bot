package telegram

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestDownloadFileUsesGetFileAndBoundedBody(t *testing.T) {
	srv, calls := newServer(t,
		func(w http.ResponseWriter, body string) {
			if !strings.Contains(body, `"file_id":"abc"`) {
				t.Errorf("getFile body: %s", body)
			}
			ok(w, `{"file_size":5,"file_path":"photos/pic.jpg"}`)
		},
		func(w http.ResponseWriter, _ string) { _, _ = io.WriteString(w, "hello") },
	)
	c := newClient(t, srv)
	data, err := c.DownloadFile(context.Background(), "abc")
	if err != nil || string(data) != "hello" {
		t.Fatalf("DownloadFile = %q, %v", data, err)
	}
	if len(*calls) != 2 || (*calls)[1].Method != "/file/bot"+testToken+"/photos/pic.jpg" {
		t.Fatalf("calls: %+v", *calls)
	}
}

func TestDownloadFileRejectsTooLargeAndUnsafePath(t *testing.T) {
	for _, tc := range []struct {
		name, result string
		want         error
	}{
		{"metadata limit", `{"file_size":20971521,"file_path":"big.bin"}`, ErrFileTooLarge},
		{"unsafe path", `{"file_size":1,"file_path":"../bad"}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, calls := newServer(t, func(w http.ResponseWriter, _ string) { ok(w, tc.result) })
			_, err := newClient(t, srv).DownloadFile(context.Background(), "abc")
			if err == nil {
				t.Fatal("accepted invalid file")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if len(*calls) != 1 {
				t.Fatalf("download was attempted: %+v", *calls)
			}
		})
	}
}

func TestDownloadFileEnforcesBodyLimit(t *testing.T) {
	srv, _ := newServer(t,
		func(w http.ResponseWriter, _ string) { ok(w, `{"file_path":"big.bin"}`) },
		func(w http.ResponseWriter, _ string) {
			_, _ = io.WriteString(w, strings.Repeat("x", int(MaxDownloadBytes)+1))
		},
	)
	_, err := newClient(t, srv).DownloadFile(context.Background(), "abc")
	if !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("error = %v, want ErrFileTooLarge", err)
	}
}

func TestDownloadFileErrorDoesNotExposeToken(t *testing.T) {
	srv, _ := newServer(t,
		func(w http.ResponseWriter, _ string) { ok(w, `{"file_path":"missing.bin"}`) },
		func(w http.ResponseWriter, _ string) { w.WriteHeader(http.StatusNotFound) },
	)
	_, err := newClient(t, srv).DownloadFile(context.Background(), "abc")
	if err == nil || strings.Contains(err.Error(), testToken) {
		t.Fatalf("unsafe download error: %v", err)
	}
}

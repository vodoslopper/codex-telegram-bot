package webretrieve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHTTPSelections(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("one\ntwo error\nthree\nfour"))
	}))
	defer s.Close()
	for _, tc := range []struct {
		q    Request
		want string
	}{
		{Request{Start: 2, End: 3}, "2: two error\n3: three\n"},
		{Request{Tail: 1}, "4: four\n"},
		{Request{Search: "error", Expect: "two"}, "2: two error\n"},
	} {
		tc.q.URL = s.URL
		r := (Retriever{Client: s.Client()}).Fetch(context.Background(), tc.q)
		if r.Error != "" || r.Content != tc.want {
			t.Fatalf("%+v", r)
		}
	}
	r := (Retriever{Client: s.Client()}).Fetch(context.Background(), Request{URL: s.URL, Expect: "absent"})
	if r.Error == "" || r.Content != "" {
		t.Fatal(r)
	}
}
func TestChallengeFallback(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(403)
		w.Write([]byte("<html>Making sure you&#39;re not a bot</html>"))
	}))
	defer s.Close()
	for _, body := range []string{"<html><pre>build completed</pre></html>", "<html>Making sure you&#39;re not a bot</html>", ""} {
		calls := 0
		r := (Retriever{Client: s.Client(), Browser: func(context.Context, string) (string, bool, error) { calls++; return body, false, nil }}).Fetch(context.Background(), Request{URL: s.URL, Expect: "build completed"})
		if calls != 1 || r.Method != "chromium-headless-shell" {
			t.Fatal(r)
		}
		if strings.Contains(body, "build completed") {
			if r.Error != "" {
				t.Fatal(r)
			}
		} else if r.Error == "" || r.Content != "" {
			t.Fatal(r)
		}
	}
}
func TestRetryBuildLogAfterChallenge(t *testing.T) {
	const challengePage = "<html>Making sure you're not a bot</html>"
	const log = "compile: success\n%check\nFAIL test_empty_queue: expected 0, got 1\n19 passed, 1 failed\nbuild wrapper exited 1"
	var published atomic.Bool
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !published.Load() {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(challengePage))
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(log))
	}))
	defer s.Close()
	retriever := Retriever{Client: s.Client(), Browser: func(context.Context, string) (string, bool, error) {
		return challengePage, false, nil
	}}
	q := Request{URL: s.URL}
	if blocked := retriever.Fetch(t.Context(), q); blocked.Error == "" || blocked.Content != "" {
		t.Fatalf("challenge exposed as evidence: %+v", blocked)
	}
	published.Store(true)
	got := retriever.Fetch(t.Context(), q)
	if got.Error != "" || !strings.Contains(got.Content, "%check\n3: FAIL test_empty_queue: expected 0, got 1") {
		t.Fatalf("retry did not retrieve the published failure: %+v", got)
	}
}

func TestLimits(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(strings.Repeat("a log line\n", MaxDownload/10+1)))
	}))
	defer s.Close()
	r := (Retriever{Client: s.Client()}).Fetch(context.Background(), Request{URL: s.URL})
	if !r.DownloadTruncated || !r.OutputTruncated || len(r.Content) > MaxOutput {
		t.Fatal(r)
	}
	r = (Retriever{Client: s.Client()}).Fetch(context.Background(), Request{URL: s.URL, Tail: 2})
	if r.Error == "" {
		t.Fatal("false tail returned")
	}
}
func TestPublicOnly(t *testing.T) {
	r := (Retriever{}).Fetch(context.Background(), Request{URL: "http://127.0.0.1/"})
	if r.Error == "" {
		t.Fatal(r)
	}
}
func TestHTTPLive(t *testing.T) {
	if os.Getenv("BOT_WEB_LIVE_TEST") != "1" {
		t.Skip("opt-in public network test")
	}
	target, expected := liveTarget(t, "BOT_WEB_TEST")
	for _, q := range []Request{
		{Start: 1, End: 4, Expect: expected},
		{Search: expected, Expect: expected},
		{Tail: 10, Expect: expected},
	} {
		q.URL = target
		r := (Retriever{}).Fetch(context.Background(), q)
		if r.Error != "" {
			t.Fatalf("%+v", r)
		}
		t.Logf("method=%s download_truncated=%v output_truncated=%v\n%s", r.Method, r.DownloadTruncated, r.OutputTruncated, r.Content)
	}
}

func TestBrowserLive(t *testing.T) {
	if os.Getenv("BOT_WEB_BROWSER_TEST") != "1" {
		t.Skip("opt-in real Chromium network test")
	}
	target, expected := liveTarget(t, "BOT_WEB_TEST")
	body, truncated, err := Browser(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	if truncated || challenge(body) || !strings.Contains(plain(body), expected) {
		t.Fatalf("browser did not retrieve complete requested build log: truncated=%v bytes=%d\n%s", truncated, len(body), plain(body[:min(4096, len(body))]))
	}
	t.Logf("real Chromium retrieved verified content: %d DOM bytes", len(body))
}

func TestChallengeLive(t *testing.T) {
	if os.Getenv("BOT_WEB_CHALLENGE_TEST") != "1" {
		t.Skip("opt-in real browser challenge fallback test")
	}
	target, expected := liveTarget(t, "BOT_WEB_CHALLENGE")
	result := (Retriever{}).Fetch(context.Background(), Request{URL: target, Expect: expected, Start: 1, End: 20})
	if result.Error != "" {
		t.Fatalf("method=%s: %s", result.Method, result.Error)
	}
	if result.Method != "chromium-headless-shell" {
		t.Fatalf("expected real challenge fallback, got %s", result.Method)
	}
	t.Logf("Challenge resolved; verified requested content; download_truncated=%v", result.DownloadTruncated)
}

func liveTarget(t *testing.T, prefix string) (string, string) {
	t.Helper()
	target, expected := os.Getenv(prefix+"_URL"), os.Getenv(prefix+"_EXPECT")
	if target == "" || expected == "" {
		t.Fatalf("set %s_URL and %s_EXPECT for content verification", prefix, prefix)
	}
	return target, expected
}

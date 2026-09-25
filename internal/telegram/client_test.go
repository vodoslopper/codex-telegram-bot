package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const testToken = "123456:AAH-secret-token-value-that-must-never-be-logged"

// recordedCall is one request the test server received.
type recordedCall struct {
	Method string
	Body   string
}

// newServer builds an httptest server that records calls and answers from a
// queue of handlers, one per request.
func newServer(t *testing.T, handlers ...func(w http.ResponseWriter, body string)) (*httptest.Server, *[]recordedCall) {
	t.Helper()
	var calls []recordedCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		calls = append(calls, recordedCall{Method: r.URL.Path, Body: string(raw)})
		idx := len(calls) - 1
		if idx < len(handlers) {
			handlers[idx](w, string(raw))
			return
		}
		// Out of scripted answers: fail loudly rather than silently returning
		// something a test did not ask for.
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"ok":false,"error_code":500,"description":"test server out of scripted answers"}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func newClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c, err := New(testToken, srv.URL, 5*time.Second, nil,
		WithMaxRetries(3), WithBackoff(5*time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func ok(w http.ResponseWriter, result string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"ok":true,"result":`+result+`}`)
}

func fail(w http.ResponseWriter, code int, description string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, `{"ok":false,"error_code":`+strconv.Itoa(code)+
		`,"description":`+jsonString(description)+`}`)
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestNewRejectsAMissingToken(t *testing.T) {
	if _, err := New("", "https://api.telegram.org", time.Second, nil); err == nil {
		t.Fatal("New accepted an empty token")
	}
}

func TestNewRejectsABadBaseURL(t *testing.T) {
	for _, base := range []string{"not a url", "://x", "ftp://example.com"} {
		if _, err := New(testToken, base, time.Second, nil); err == nil {
			t.Errorf("New accepted the base URL %q", base)
		}
	}
}

func TestGetMe(t *testing.T) {
	srv, calls := newServer(t, func(w http.ResponseWriter, body string) {
		ok(w, `{"id":42,"is_bot":true,"first_name":"Codex","username":"codex_test_bot"}`)
	})
	c := newClient(t, srv)

	me, err := c.GetMe(context.Background())
	if err != nil {
		t.Fatalf("GetMe: %v", err)
	}
	if me.ID != 42 || me.Username != "codex_test_bot" || !me.IsBot {
		t.Errorf("GetMe = %+v", me)
	}
	if got := (*calls)[0].Method; !strings.HasSuffix(got, "/getMe") {
		t.Errorf("the request went to %q", got)
	}
}

func TestGetUpdatesParsesAndFiltersToMessages(t *testing.T) {
	var seenBody map[string]any
	srv, _ := newServer(t, func(w http.ResponseWriter, body string) {
		_ = json.Unmarshal([]byte(body), &seenBody)
		ok(w, `[
			{"update_id":10,"message":{"message_id":10,"date":1700000000,"text":"hello",
			  "from":{"id":111,"is_bot":false,"first_name":"A"},
			  "chat":{"id":111,"type":"private"}}},
			{"update_id":11,"message":{"message_id":11,"date":1700000001,"text":"in a topic",
			  "message_thread_id":42,
			  "from":{"id":111,"is_bot":false,"first_name":"A"},
			  "chat":{"id":111,"type":"private"}}},
			{"update_id":12,"message":{"message_id":12,"date":1700000002,"text":"group noise",
			  "from":{"id":222,"is_bot":false,"first_name":"B"},
			  "chat":{"id":-100222,"type":"supergroup"}}}
		]`)
	})
	c := newClient(t, srv)

	updates, err := c.GetUpdates(context.Background(), 10, 25*time.Second, 100)
	if err != nil {
		t.Fatalf("GetUpdates: %v", err)
	}
	if len(updates) != 3 {
		t.Fatalf("got %d updates, want 3", len(updates))
	}
	if updates[0].Message.Text != "hello" || updates[0].Message.From.ID != 111 {
		t.Errorf("update 10 = %+v", updates[0].Message)
	}
	if !updates[0].Message.Chat.IsPrivate() {
		t.Error("a private chat was not recognised")
	}
	if updates[2].Message.Chat.IsPrivate() {
		t.Error("a supergroup was reported as private; groups must be rejected")
	}
	if got := updates[1].Message.ThreadID(); got != 42 {
		t.Errorf("ThreadID = %d, want 42", got)
	}
	if got := updates[0].Message.ThreadID(); got != 0 {
		t.Errorf("ThreadID = %d, want the 0 sentinel for a chat without topics", got)
	}
	if updates[0].Kind() != "message" {
		t.Errorf("Kind = %q", updates[0].Kind())
	}

	// The request must ask only for messages and pass the offset through.
	if got := seenBody["offset"]; got != float64(10) {
		t.Errorf("offset in the request = %v, want 10", got)
	}
	if got := seenBody["timeout"]; got != float64(25) {
		t.Errorf("timeout in the request = %v, want 25", got)
	}
	allowed, _ := seenBody["allowed_updates"].([]any)
	if len(allowed) != 1 || allowed[0] != "message" {
		t.Errorf("allowed_updates = %v, want only \"message\"", allowed)
	}
}

func TestSendMessageBody(t *testing.T) {
	var body map[string]any
	srv, _ := newServer(t, func(w http.ResponseWriter, raw string) {
		_ = json.Unmarshal([]byte(raw), &body)
		ok(w, `{"message_id":77,"chat":{"id":111,"type":"private"},"date":1700000000}`)
	})
	c := newClient(t, srv)

	sent, err := c.SendMessage(context.Background(), 111, 42, "the answer")
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if sent.MessageID != 77 {
		t.Errorf("MessageID = %d", sent.MessageID)
	}
	if body["chat_id"] != float64(111) || body["text"] != "the answer" {
		t.Errorf("request body = %v", body)
	}
	if body["message_thread_id"] != float64(42) {
		t.Errorf("the reply is not addressed to the topic: %v", body)
	}
	opts, _ := body["link_preview_options"].(map[string]any)
	if opts == nil || opts["is_disabled"] != true {
		t.Errorf("link previews are not disabled: %v", body)
	}
}

func TestSendMessageOmitsThreadZero(t *testing.T) {
	var body map[string]any
	srv, _ := newServer(t, func(w http.ResponseWriter, raw string) {
		_ = json.Unmarshal([]byte(raw), &body)
		ok(w, `{"message_id":1}`)
	})
	c := newClient(t, srv)
	if _, err := c.SendMessage(context.Background(), 111, 0, "hi"); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if _, present := body["message_thread_id"]; present {
		t.Errorf("message_thread_id was sent for a chat without topics: %v", body)
	}
}

func TestSendMessageRejectsEmptyText(t *testing.T) {
	srv, calls := newServer(t, func(w http.ResponseWriter, _ string) { ok(w, `{}`) })
	c := newClient(t, srv)
	if _, err := c.SendMessage(context.Background(), 1, 0, ""); err == nil {
		t.Fatal("SendMessage accepted an empty message")
	}
	if len(*calls) != 0 {
		t.Error("an empty message was sent to Telegram anyway")
	}
}

func TestRetryAfterIsHonoured(t *testing.T) {
	var attempts atomic.Int32
	srv, _ := newServer(t,
		func(w http.ResponseWriter, _ string) {
			attempts.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"ok":false,"error_code":429,"description":"Too Many Requests: retry after 1",`+
				`"parameters":{"retry_after":1}}`)
		},
		func(w http.ResponseWriter, _ string) {
			attempts.Add(1)
			ok(w, `{"id":42,"is_bot":true,"username":"codex_test_bot"}`)
		},
	)
	c := newClient(t, srv)

	start := time.Now()
	if _, err := c.GetMe(context.Background()); err != nil {
		t.Fatalf("GetMe: %v", err)
	}
	if attempts.Load() != 2 {
		t.Errorf("attempts = %d, want 2", attempts.Load())
	}
	// Telegram said one second, so the bot must wait at least that long rather
	// than hammering a flood-limited API.
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Errorf("the 429 retry_after was not honoured: waited %s", elapsed)
	}
}

func TestServerErrorIsRetried(t *testing.T) {
	var attempts atomic.Int32
	srv, _ := newServer(t,
		func(w http.ResponseWriter, _ string) { attempts.Add(1); fail(w, 502, "bad gateway") },
		func(w http.ResponseWriter, _ string) { attempts.Add(1); fail(w, 500, "boom") },
		func(w http.ResponseWriter, _ string) { attempts.Add(1); ok(w, `{"id":42}`) },
	)
	c := newClient(t, srv)
	if _, err := c.GetMe(context.Background()); err != nil {
		t.Fatalf("GetMe: %v", err)
	}
	if attempts.Load() != 3 {
		t.Errorf("attempts = %d, want 3", attempts.Load())
	}
}

func TestClientErrorIsNotRetried(t *testing.T) {
	var attempts atomic.Int32
	srv, _ := newServer(t, func(w http.ResponseWriter, _ string) {
		attempts.Add(1)
		fail(w, 400, "Bad Request: chat not found")
	})
	c := newClient(t, srv)

	_, err := c.SendMessage(context.Background(), 1, 0, "hi")
	if err == nil {
		t.Fatal("SendMessage succeeded on a 400")
	}
	if attempts.Load() != 1 {
		t.Errorf("attempts = %d; a 400 can never succeed, so retrying it wastes time", attempts.Load())
	}
	if !IsNotFound(err) {
		t.Errorf("IsNotFound(%v) = false", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode != 400 {
		t.Errorf("the error is not an APIError with code 400: %v", err)
	}
}

func TestUnauthorizedIsClassified(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, _ string) {
		fail(w, 401, "Unauthorized")
	})
	c := newClient(t, srv)
	_, err := c.GetMe(context.Background())
	if !IsUnauthorized(err) {
		t.Errorf("IsUnauthorized(%v) = false", err)
	}
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Temporary() {
		t.Error("a 401 is reported as temporary, so the bot would retry it forever")
	}
}

func TestForbiddenIsClassified(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, _ string) {
		fail(w, 403, "Forbidden: bot was blocked by the user")
	})
	c := newClient(t, srv)
	_, err := c.SendMessage(context.Background(), 1, 0, "hi")
	if !IsForbidden(err) {
		t.Errorf("IsForbidden(%v) = false", err)
	}
}

// TestTokenNeverAppearsInAnError is the leak test: the token is a path element
// of every request URL, so any error that quoted the URL — which is exactly what
// net/http does for transport failures — would put it in the log.
func TestTokenNeverAppearsInAnError(t *testing.T) {
	// One server that always fails, with the token echoed in the description as
	// a worst case.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"ok":false,"error_code":400,"description":"bad request for `+
			testToken+`"}`)
	}))
	defer srv.Close()
	c := newClient(t, srv)

	calls := map[string]func() error{
		"getMe":          func() error { _, err := c.GetMe(context.Background()); return err },
		"sendMessage":    func() error { _, err := c.SendMessage(context.Background(), 1, 0, "x"); return err },
		"getUpdates":     func() error { _, err := c.GetUpdates(context.Background(), 0, time.Second, 10); return err },
		"sendChatAction": func() error { return c.SendChatAction(context.Background(), 1, 0, "typing") },
		"deleteWebhook":  func() error { return c.DeleteWebhook(context.Background(), false) },
		"getWebhookInfo": func() error { _, err := c.GetWebhookInfo(context.Background()); return err },
	}
	for name, call := range calls {
		err := call()
		if err == nil {
			t.Errorf("%s succeeded against a server that always fails", name)
			continue
		}
		if strings.Contains(err.Error(), testToken) || strings.Contains(err.Error(), "secret-token-value") {
			t.Errorf("%s leaked the token into an error: %v", name, err)
		}
		if !strings.Contains(err.Error(), "[redacted-token]") {
			t.Errorf("%s dropped the echoed token without leaving a marker: %v", name, err)
		}
	}

	// A closed server produces transport errors, the shape most likely to quote
	// the URL.
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	dc, err := New(testToken, deadURL, 2*time.Second, nil, WithMaxRetries(0))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	terr := func() error { _, err := dc.GetMe(context.Background()); return err }()
	if terr == nil {
		t.Fatal("GetMe succeeded against a closed server")
	}
	if strings.Contains(terr.Error(), testToken) || strings.Contains(terr.Error(), "secret-token-value") {
		t.Errorf("the token leaked into a transport error: %v", terr)
	}
	if !strings.Contains(terr.Error(), "connection refused") {
		t.Errorf("the transport error lost its useful part: %v", terr)
	}

	// A cancelled context must still be recognisable through the wrapper, or the
	// bot could not tell a shutdown from a network failure.
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	cerr := func() error { _, err := dc.GetMe(cctx); return err }()
	if !errors.Is(cerr, context.Canceled) {
		t.Errorf("a cancelled call is not recognisable as context.Canceled: %v", cerr)
	}
	if strings.Contains(fmt.Sprint(cerr), testToken) {
		t.Errorf("the cancelled-call error leaked the token: %v", cerr)
	}
}

func TestRedactDescriptionMasksAToken(t *testing.T) {
	in := "Unauthorized: bot " + testToken + " is not valid"
	got := redactDescription(in)
	if strings.Contains(got, "secret-token-value") {
		t.Errorf("redactDescription left the token in place: %q", got)
	}
	if !strings.Contains(got, "[redacted-token]") {
		t.Errorf("redactDescription = %q, want a marker", got)
	}
}

func TestNonJSONResponseIsExplained(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "<html>not the bot api</html>")
	}))
	defer srv.Close()
	c := newClient(t, srv)

	_, err := c.GetMe(context.Background())
	if err == nil {
		t.Fatal("GetMe accepted an HTML response")
	}
	if !strings.Contains(err.Error(), "BOT_TELEGRAM_API_BASE") {
		t.Errorf("the error does not point at the likely cause: %v", err)
	}
}

func TestCancellationStopsRetries(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		fail(w, 500, "boom")
	}))
	defer srv.Close()

	// A backoff long enough that the deadline lands inside it: the point is that
	// the wait is interruptible, so a shutdown does not sit out a retry.
	c, err := New(testToken, srv.URL, 5*time.Second, nil, WithMaxRetries(5), WithBackoff(5*time.Second))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := c.GetMe(ctx); err == nil {
		t.Fatal("GetMe succeeded although every attempt failed")
	}
	if attempts.Load() != 1 {
		t.Errorf("attempts = %d; the retry loop ignored the cancelled context", attempts.Load())
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("the cancelled call took %s to give up", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Logf("note: the returned error is %v", err)
	}
}

func TestWebhookMethods(t *testing.T) {
	srv, calls := newServer(t,
		func(w http.ResponseWriter, _ string) {
			ok(w, `{"url":"https://example.invalid/hook","pending_update_count":3,"last_error_message":"x"}`)
		},
		func(w http.ResponseWriter, _ string) { ok(w, `true`) },
		func(w http.ResponseWriter, _ string) { ok(w, `"typing"`) },
	)
	c := newClient(t, srv)
	ctx := context.Background()

	info, err := c.GetWebhookInfo(ctx)
	if err != nil {
		t.Fatalf("GetWebhookInfo: %v", err)
	}
	if info.URL != "https://example.invalid/hook" || info.PendingUpdateCount != 3 {
		t.Errorf("GetWebhookInfo = %+v", info)
	}
	if err := c.DeleteWebhook(ctx, false); err != nil {
		t.Fatalf("DeleteWebhook: %v", err)
	}
	if err := c.SendChatAction(ctx, 111, 42, "typing"); err != nil {
		t.Fatalf("SendChatAction: %v", err)
	}

	// The webhook must not be deleted with drop_pending_updates, or messages
	// sent while the bot was down would be thrown away.
	if !strings.Contains((*calls)[1].Body, `"drop_pending_updates":false`) {
		t.Errorf("deleteWebhook body = %s, want drop_pending_updates false", (*calls)[1].Body)
	}
	if !strings.HasSuffix((*calls)[1].Method, "/deleteWebhook") {
		t.Errorf("the second call went to %q", (*calls)[1].Method)
	}
	if !strings.Contains((*calls)[2].Body, `"message_thread_id":42`) {
		t.Errorf("sendChatAction body = %s, want the topic id", (*calls)[2].Body)
	}
}

func TestUpdateKind(t *testing.T) {
	cases := []struct {
		update Update
		want   string
	}{
		{Update{Message: &Message{}}, "message"},
		{Update{EditedMessage: &Message{}}, "edited_message"},
		{Update{ChannelPost: &Message{}}, "channel_post"},
		{Update{CallbackQuery: &struct {
			ID string `json:"id"`
		}{ID: "x"}}, "callback_query"},
		{Update{MyChatMember: &struct{}{}}, "my_chat_member"},
		{Update{}, "other"},
	}
	for _, c := range cases {
		if got := c.update.Kind(); got != c.want {
			t.Errorf("Kind() = %q, want %q", got, c.want)
		}
	}
}

func TestChatIsPrivate(t *testing.T) {
	for typ, want := range map[string]bool{
		"private":    true,
		"group":      false,
		"supergroup": false,
		"channel":    false,
		"":           false,
	} {
		if got := (Chat{Type: typ}).IsPrivate(); got != want {
			t.Errorf("Chat{Type:%q}.IsPrivate() = %v, want %v", typ, got, want)
		}
	}
}

package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// DefaultMaxRetries bounds how often one API call is retried.
const DefaultMaxRetries = 4

// maxResponseBody caps how much of an answer we read. Telegram responses here
// are tiny; the cap only protects against a hostile or broken endpoint.
const maxResponseBody = 1 << 20

// Errors returned by the client.
var (
	// ErrNoToken means the client was built without a token.
	ErrNoToken = errors.New("telegram: no bot token configured")
)

// APIError is a refusal from the Bot API. ErrorCode is Telegram's HTTP status.
type APIError struct {
	Method      string
	ErrorCode   int
	Description string
	RetryAfter  time.Duration
}

func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "telegram: %s failed with code %d", e.Method, e.ErrorCode)
	if e.Description != "" {
		// Redacted here as well as where the error is built, so an APIError
		// cannot leak a token no matter who constructed it.
		fmt.Fprintf(&b, ": %s", redactDescription(e.Description))
	}
	if e.RetryAfter > 0 {
		fmt.Fprintf(&b, " (retry after %s)", e.RetryAfter)
	}
	return b.String()
}

// Temporary reports whether retrying could plausibly succeed. 429 and 5xx can;
// 400/401/403/404 cannot, and retrying those just burns time.
func (e *APIError) Temporary() bool {
	return e.ErrorCode == 429 || e.ErrorCode >= 500
}

// Client talks to the Bot API.
type Client struct {
	token   string
	baseURL string
	http    *http.Client
	log     *slog.Logger

	maxRetries int
	backoff    time.Duration
}

// Option customises a Client.
type Option func(*Client)

// WithMaxRetries sets how many times a transient failure is retried.
func WithMaxRetries(n int) Option {
	return func(c *Client) {
		if n >= 0 {
			c.maxRetries = n
		}
	}
}

// WithBackoff sets the base delay for exponential backoff.
func WithBackoff(d time.Duration) Option {
	return func(c *Client) {
		if d > 0 {
			c.backoff = d
		}
	}
}

// New builds a client. baseURL is the Bot API root, normally
// https://api.telegram.org; tests point it at an httptest server.
func New(token, baseURL string, httpTimeout time.Duration, log *slog.Logger, opts ...Option) (*Client, error) {
	if token == "" {
		return nil, ErrNoToken
	}
	if baseURL == "" {
		baseURL = "https://api.telegram.org"
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("telegram: invalid API base %q", baseURL)
	}
	// Reject a wrong scheme here rather than at the first request: "ftps://" or
	// a bare hostname in BOT_TELEGRAM_API_BASE would otherwise produce a
	// confusing transport error on every call.
	if u.Scheme != "https" && u.Scheme != "http" {
		return nil, fmt.Errorf("telegram: API base %q must be an http(s) URL", baseURL)
	}
	if log == nil {
		log = slog.Default()
	}
	c := &Client{
		token:   token,
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: httpTimeout},
		log:     log,

		maxRetries: DefaultMaxRetries,
		backoff:    500 * time.Millisecond,
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// endpoint builds the method URL. The token is in the path, as the Bot API
// requires; this string must never be logged.
func (c *Client) endpoint(method string) string {
	return c.baseURL + "/bot" + c.token + "/" + method
}

// call POSTs body to method and decodes the envelope into out.
//
// Retries are bounded and only happen for network failures, 5xx and 429. A 429
// carries Telegram's own retry_after, which is honoured exactly rather than
// guessed, because guessing during a flood-limit makes things worse.
func (c *Client) call(ctx context.Context, method string, body any, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("telegram: encode %s request: %w", method, err)
	}

	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		retryAfter, err := c.once(ctx, method, payload, out)
		if err == nil {
			return nil
		}

		var apiErr *APIError
		transient := errors.As(err, &apiErr) && apiErr.Temporary()
		if !transient || attempt >= c.maxRetries {
			return err
		}

		delay := c.backoffFor(attempt)
		if retryAfter > 0 {
			delay = retryAfter
		}
		c.log.Warn("telegram call failed, retrying",
			"method", method, "attempt", attempt+1, "delay", delay.Round(time.Millisecond),
			"error", redactDescription(err.Error()))

		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
}

// backoffFor is exponential backoff with full jitter, capped at 30s. Jitter
// matters when several bots behind one IP hit a Telegram outage together.
func (c *Client) backoffFor(attempt int) time.Duration {
	d := c.backoff << min(attempt, 6)
	if d > 30*time.Second {
		d = 30 * time.Second
	}
	return time.Duration(rand.Int64N(int64(d)/2+1)) + d/2
}

// once performs a single HTTP round trip. It returns the retry_after hint from
// a 429 alongside the error so call can honour it.
func (c *Client) once(ctx context.Context, method string, payload []byte, out any) (time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(method), bytes.NewReader(payload))
	if err != nil {
		return 0, fmt.Errorf("telegram: build %s request: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "codex-telegram-bot/1.0")

	resp, err := c.http.Do(req)
	if err != nil {
		// A transport failure is transient by nature. The error is redacted
		// first: net/http wraps failures in a *url.Error whose message quotes the
		// full URL, and the URL contains the bot token.
		return 0, c.transportError(method, err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))
	if err != nil {
		return 0, fmt.Errorf("telegram: read %s response: %w", method, err)
	}

	var env apiResponse[json.RawMessage]
	if jerr := json.Unmarshal(raw, &env); jerr != nil {
		// Not JSON at all: a proxy, a captive portal or a wrong base URL.
		return 0, fmt.Errorf("telegram: %s returned a non-JSON response (HTTP %d); is BOT_TELEGRAM_API_BASE correct?",
			method, resp.StatusCode)
	}
	if !env.OK {
		apiErr := &APIError{
			Method: method,
			// Telegram echoes back what it was handed in some error descriptions,
			// and a request URL contains the token. Redact before storing, so
			// the secret cannot travel along with the error into a log line or a
			// chat message.
			Description: c.redact(env.Description),
			ErrorCode:   env.ErrorCode,
		}
		if apiErr.ErrorCode == 0 {
			apiErr.ErrorCode = resp.StatusCode
		}
		if env.Parameters != nil && env.Parameters.RetryAfter > 0 {
			apiErr.RetryAfter = time.Duration(env.Parameters.RetryAfter) * time.Second
		}
		return apiErr.RetryAfter, apiErr
	}
	if out == nil {
		return 0, nil
	}
	if jerr := json.Unmarshal(env.Result, out); jerr != nil {
		return 0, fmt.Errorf("telegram: decode %s result: %w", method, jerr)
	}
	return 0, nil
}

// tokenPattern matches a Bot API token: "<bot id>:<35-ish char secret>".
var tokenPattern = regexp.MustCompile(`\b\d{5,}:[A-Za-z0-9_\-]{20,}\b`)

// redactDescription scrubs a token that came back inside an error string. The
// Bot API does not normally echo one, but a proxy or a misconfigured base URL
// can, and this text goes into the log.
func redactDescription(s string) string {
	return tokenPattern.ReplaceAllString(s, "[redacted-token]")
}

// redactedError reports a transport failure without quoting the request URL.
//
// The token is a path element of every request URL, and net/http includes the
// URL in the errors it returns. Wrapping rather than replacing keeps
// errors.Is(err, context.Canceled) and friends working, while Error() — which is
// what logs and Telegram messages use — can never print the secret.
type redactedError struct {
	msg string
	err error
}

func (e *redactedError) Error() string { return e.msg }
func (e *redactedError) Unwrap() error { return e.err }

// transportError builds a redacted transport error for a method.
func (c *Client) transportError(method string, err error) error {
	return &redactedError{msg: fmt.Sprintf("telegram: %s: %s", method, c.redact(err.Error())), err: err}
}

// redact removes this client's token, and anything else shaped like a token,
// from a string that is about to become part of an error or a log line.
func (c *Client) redact(s string) string {
	if c.token != "" {
		s = strings.ReplaceAll(s, c.token, "[redacted-token]")
	}
	return tokenPattern.ReplaceAllString(s, "[redacted-token]")
}

// --- methods ---------------------------------------------------------------

// GetMe validates the token and returns the bot's own identity. The username is
// needed to recognise "/command@thisbot" in group-style addressing.
func (c *Client) GetMe(ctx context.Context) (*User, error) {
	var u User
	if err := c.call(ctx, "getMe", struct{}{}, &u); err != nil {
		return nil, err
	}
	return &u, nil
}

// GetUpdates long-polls for updates at or after offset.
//
// An empty slice with a nil error is the normal "nothing happened" answer, and
// is what a long-poll timeout looks like.
func (c *Client) GetUpdates(ctx context.Context, offset int64, pollTimeout time.Duration, limit int) ([]Update, error) {
	body := struct {
		Offset         int64    `json:"offset"`
		Limit          int      `json:"limit,omitempty"`
		Timeout        int      `json:"timeout,omitempty"`
		AllowedUpdates []string `json:"allowed_updates"`
	}{
		Offset:  offset,
		Limit:   limit,
		Timeout: int(pollTimeout.Seconds()),
		// Only plain messages are handled, so only plain messages are
		// requested. Everything else would be delivered, decoded, dropped and
		// would still advance the offset — pure noise.
		AllowedUpdates: []string{"message"},
	}
	var updates []Update
	if err := c.call(ctx, "getUpdates", body, &updates); err != nil {
		return nil, err
	}
	return updates, nil
}

// SendMessage sends one message and returns its id.
//
// Text longer than Telegram's limit is rejected by the API, so callers must
// split first; internal/textsplit does that. The check here is a second line of
// defence against a caller that forgot.
func (c *Client) SendMessage(ctx context.Context, chatID, threadID int64, text string) (*SentMessage, error) {
	if text == "" {
		return nil, errors.New("telegram: refusing to send an empty message")
	}
	body := SendMessageParams{ChatID: chatID, Text: text}
	if threadID != 0 {
		body.MessageThreadID = threadID
	}
	body.LinkPreview = &LinkPreviewOpts{IsDisabled: true}

	var sent SentMessage
	if err := c.call(ctx, "sendMessage", body, &sent); err != nil {
		return nil, err
	}
	return &sent, nil
}

// SendChatAction shows "typing…" for about five seconds. Failures are not
// interesting — the indicator is cosmetic — so callers usually log and ignore.
func (c *Client) SendChatAction(ctx context.Context, chatID, threadID int64, action string) error {
	body := struct {
		ChatID          int64  `json:"chat_id"`
		Action          string `json:"action"`
		MessageThreadID int64  `json:"message_thread_id,omitempty"`
	}{ChatID: chatID, Action: action}
	if threadID != 0 {
		body.MessageThreadID = threadID
	}
	return c.call(ctx, "sendChatAction", body, nil)
}

// DeleteWebhook removes any webhook so getUpdates starts working again. It is
// idempotent: with no webhook set, Telegram answers ok.
func (c *Client) DeleteWebhook(ctx context.Context, dropPending bool) error {
	body := struct {
		DropPendingUpdates bool `json:"drop_pending_updates"`
	}{DropPendingUpdates: dropPending}
	return c.call(ctx, "deleteWebhook", body, nil)
}

// GetWebhookInfo reports the current webhook configuration.
func (c *Client) GetWebhookInfo(ctx context.Context) (*WebhookInfo, error) {
	var info WebhookInfo
	if err := c.call(ctx, "getWebhookInfo", struct{}{}, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// IsNotFound reports whether err is Telegram's "message to send not found" or
// "chat not found", which happen when the user blocked the bot. Retrying those
// is pointless.
func IsNotFound(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.ErrorCode == http.StatusNotFound ||
		strings.Contains(strings.ToLower(apiErr.Description), "not found")
}

// IsForbidden reports whether the bot was blocked by the user or kicked from
// the chat.
func IsForbidden(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.ErrorCode == http.StatusForbidden
}

// IsUnauthorized reports a bad token, which is fatal at startup and never worth
// retrying later.
func IsUnauthorized(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode == http.StatusUnauthorized
}

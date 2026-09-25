// Package telegram is a minimal Bot API client: exactly the handful of methods
// a long-polling bot needs, over net/http, with no framework.
//
// The bot uses getUpdates only. There is no listener, no TLS certificate to
// serve, no inbound firewall rule, and no public DNS name — the process makes
// outbound HTTPS calls and nothing else. A webhook and a poller are mutually
// exclusive on Telegram's side, which is why the bot deletes any webhook at
// startup (see Config.DeleteWebhook).
//
// The token is never put in a log line, an error string or a URL that escapes
// this package: every request is built here and every failure is reported as a
// method name plus Telegram's own description.
package telegram

// Update is one item from getUpdates.
//
// Only the fields this bot acts on are decoded; Telegram sends many more and
// encoding/json ignores what is not declared here.
type Update struct {
	UpdateID int64    `json:"update_id"`
	Message  *Message `json:"message"`
	// Everything below is decoded so the bot can tell "not a message" apart
	// from "a message with no text", and log the difference. None of it is
	// acted on.
	EditedMessage *Message `json:"edited_message"`
	ChannelPost   *Message `json:"channel_post"`
	CallbackQuery *struct {
		ID string `json:"id"`
	} `json:"callback_query"`
	MyChatMember *struct{} `json:"my_chat_member"`
}

// Kind names the update for the deduplication audit trail.
func (u Update) Kind() string {
	switch {
	case u.Message != nil:
		return "message"
	case u.EditedMessage != nil:
		return "edited_message"
	case u.ChannelPost != nil:
		return "channel_post"
	case u.CallbackQuery != nil:
		return "callback_query"
	case u.MyChatMember != nil:
		return "my_chat_member"
	default:
		return "other"
	}
}

// Message is a Telegram message.
type Message struct {
	MessageID       int64  `json:"message_id"`
	MessageThreadID int64  `json:"message_thread_id"`
	Date            int64  `json:"date"`
	Text            string `json:"text"`
	From            *User  `json:"from"`
	Chat            Chat   `json:"chat"`
}

// ThreadID returns the topic id, or 0 when the chat has no topics.
//
// 0 is the sentinel for "no topic" throughout the bot: it is not a valid
// message_thread_id, so a chat without topics and a chat with topic 0 cannot
// exist at the same time.
func (m *Message) ThreadID() int64 {
	if m == nil {
		return 0
	}
	return m.MessageThreadID
}

// User is the sender. ID is the only field used for any authorization
// decision.
type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username"`
}

// Chat is where the message was sent.
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// IsPrivate reports whether this is a one-to-one chat. Groups, supergroups and
// channels are rejected: an allowlist of user ids says nothing about who else
// can read the room, and a Codex reply in a group leaks the operator's work.
func (c Chat) IsPrivate() bool { return c.Type == "private" }

// WebhookInfo is the answer to getWebhookInfo.
type WebhookInfo struct {
	URL                string `json:"url"`
	PendingUpdateCount int    `json:"pending_update_count"`
	LastErrorMessage   string `json:"last_error_message"`
}

// SentMessage is what sendMessage returns.
type SentMessage struct {
	MessageID int64 `json:"message_id"`
	Chat      Chat  `json:"chat"`
	Date      int64 `json:"date"`
}

// SendMessageParams is the request body for sendMessage.
type SendMessageParams struct {
	ChatID          int64            `json:"chat_id"`
	Text            string           `json:"text"`
	MessageThreadID int64            `json:"message_thread_id,omitempty"`
	LinkPreview     *LinkPreviewOpts `json:"link_preview_options,omitempty"`
}

// LinkPreviewOpts disables link previews, so a URL that happens to appear in a
// Codex reply is not fetched and rendered by Telegram clients.
type LinkPreviewOpts struct {
	IsDisabled bool `json:"is_disabled"`
}

// apiResponse is the envelope every Bot API method answers with.
type apiResponse[T any] struct {
	OK          bool            `json:"ok"`
	Result      T               `json:"result"`
	ErrorCode   int             `json:"error_code"`
	Description string          `json:"description"`
	Parameters  *responseParams `json:"parameters"`
}

type responseParams struct {
	RetryAfter int `json:"retry_after"`
}

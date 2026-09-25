package codexcli

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Event types the bot understands. Anything outside this list is counted and
// ignored: Codex adds event kinds between releases, and a new kind must not
// break a working bot. What must never happen is an unknown event being
// forwarded to Telegram, which is why the parser is a whitelist rather than a
// pass-through.
//
// Verified against codex-cli 0.156.1 and the published JSONL sample:
//
//	{"type":"thread.started","thread_id":"0199a213-81c0-7800-8aa1-bbab2a035a53"}
//	{"type":"turn.started"}
//	{"type":"item.started","item":{"id":"item_1","type":"command_execution",...}}
//	{"type":"item.completed","item":{"id":"item_3","type":"agent_message","text":"..."}}
//	{"type":"turn.completed","usage":{"input_tokens":24763,...}}
const (
	EventThreadStarted = "thread.started"
	EventTurnStarted   = "turn.started"
	EventTurnCompleted = "turn.completed"
	EventTurnFailed    = "turn.failed"
	EventItemStarted   = "item.started"
	EventItemCreated   = "item.created"
	EventItemUpdated   = "item.updated"
	EventItemCompleted = "item.completed"
	EventError         = "error"
)

// knownEvents is the whitelist.
var knownEvents = map[string]struct{}{
	EventThreadStarted: {},
	EventTurnStarted:   {},
	EventTurnCompleted: {},
	EventTurnFailed:    {},
	EventItemStarted:   {},
	EventItemCreated:   {},
	EventItemUpdated:   {},
	EventItemCompleted: {},
	EventError:         {},
}

// Item types the bot looks at. Only agent_message carries the reply.
const (
	ItemAgentMessage = "agent_message"
	ItemErrorMessage = "error_message"
	ItemReasoning    = "reasoning"
)

// Event is one JSONL line. Only the fields the bot acts on are declared;
// Codex sends more and they are ignored.
type Event struct {
	Type     string        `json:"type"`
	ThreadID string        `json:"thread_id"`
	Message  string        `json:"message"`
	Item     *Item         `json:"item"`
	Usage    *Usage        `json:"usage"`
	Error    *ErrorPayload `json:"error"`
}

// Item is the payload of item.* events.
type Item struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Text   string `json:"text"`
	Status string `json:"status"`
}

// Usage is the token accounting from turn.completed.
type Usage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	ReasoningOutputTokens int64 `json:"reasoning_output_tokens"`
}

// ErrorPayload is the structured error payload of turn.failed.
type ErrorPayload struct {
	Message string `json:"message"`
	Code    string `json:"code"`
	Type    string `json:"type"`
}

// Accumulator folds a JSONL stream into a verdict.
//
// It is deliberately tolerant of individual bad lines and strict about the
// result: a stream that ends without turn.completed is a failure no matter how
// much plausible text it contained.
type Accumulator struct {
	ThreadID    string
	TurnStarted bool
	Completed   bool
	Failed      bool

	// Reply is the text of the last agent_message item that completed. Codex
	// normally emits exactly one per turn; if it emits several, the last one is
	// the final answer.
	Reply         string
	AgentMessages int

	Usage Usage

	// Diagnostics. ErrorNotes collects the "message" of error events and
	// turn.failed payloads.
	ErrorNotes []string

	Items           int
	UnknownEvents   int
	UnknownExamples []string
	MalformedLines  int
	OversizedLines  int
}

// maxErrorNotes bounds how much diagnostic text is retained.
const maxErrorNotes = 8

// maxUnknownExamples bounds the unknown-event samples kept for logging.
const maxUnknownExamples = 5

// Handle consumes one JSONL line, without the trailing newline.
//
// It never returns an error: a malformed line is counted and skipped, because
// giving up on the whole stream over one bad line would throw away an otherwise
// complete answer. Whether the turn succeeded is decided by Verdict.
func (a *Accumulator) Handle(line []byte) {
	trimmed := strings.TrimSpace(string(line))
	if trimmed == "" {
		return
	}
	var ev Event
	if err := json.Unmarshal([]byte(trimmed), &ev); err != nil {
		a.MalformedLines++
		return
	}
	if ev.Type == "" {
		a.MalformedLines++
		return
	}
	if _, ok := knownEvents[ev.Type]; !ok {
		a.UnknownEvents++
		if len(a.UnknownExamples) < maxUnknownExamples {
			a.UnknownExamples = append(a.UnknownExamples, ev.Type)
		}
		return
	}

	switch ev.Type {
	case EventThreadStarted:
		// Recorded even if the turn later fails: the thread exists on disk, and
		// keeping the id is what lets a retry resume instead of starting over.
		if ev.ThreadID != "" && a.ThreadID == "" {
			a.ThreadID = ev.ThreadID
		}
	case EventTurnStarted:
		a.TurnStarted = true
	case EventTurnCompleted:
		a.Completed = true
		if ev.Usage != nil {
			a.Usage = *ev.Usage
		}
	case EventTurnFailed:
		a.Failed = true
		a.addNote(errorText(ev))
	case EventItemStarted, EventItemCreated, EventItemUpdated:
		a.Items++
	case EventItemCompleted:
		a.Items++
		if ev.Item == nil {
			return
		}
		switch ev.Item.Type {
		case ItemAgentMessage:
			if t := strings.TrimSpace(ev.Item.Text); t != "" {
				a.Reply = t
				a.AgentMessages++
			}
		case ItemErrorMessage:
			a.addNote(ev.Item.Text)
		case ItemReasoning:
			// Hidden reasoning is never surfaced. It is not stored, not
			// logged, and not sent to Telegram.
		}
	case EventError:
		// An "error" event is not necessarily fatal: Codex uses it for
		// transient stream problems too, e.g.
		//   {"type":"error","message":"Reconnecting... 2/5 (unexpected status 401 ...)"}
		// and may still recover and complete the turn. So it is collected as a
		// diagnostic and the verdict is left to turn.completed/turn.failed and
		// the exit code.
		if n := errorText(ev); n != "" {
			// Codex's "Reading additional input from stdin" is a harmless
			// progress line on a stream that is about to work; keeping it would
			// push a real reason out of the bounded note list.
			if strings.Contains(n, "Reading additional input from stdin") {
				return
			}
			a.addNote(n)
		}
	}
}

func (a *Accumulator) addNote(s string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return
	}
	if len(a.ErrorNotes) >= maxErrorNotes {
		return
	}
	a.ErrorNotes = append(a.ErrorNotes, s)
}

// errorText extracts a human-readable reason from an error or turn.failed event,
// looking in the places Codex has been observed to put one.
func errorText(ev Event) string {
	if ev.Error != nil {
		switch {
		case ev.Error.Message != "":
			return ev.Error.Message
		case ev.Error.Code != "":
			return "code " + ev.Error.Code
		case ev.Error.Type != "":
			return ev.Error.Type
		}
	}
	if ev.Message != "" {
		return ev.Message
	}
	return ""
}

// Verdict is the outcome of a parsed stream.
type Verdict int

// Verdict values.
const (
	// VerdictOK means turn.completed arrived with a final agent message.
	VerdictOK Verdict = iota
	// VerdictTurnFailed means Codex reported turn.failed.
	VerdictTurnFailed
	// VerdictNoThread means no thread.started was seen, so there is nothing to
	// resume later.
	VerdictNoThread
	// VerdictIncomplete means the stream ended without turn.completed.
	VerdictIncomplete
	// VerdictNoReply means the turn completed but produced no agent message.
	VerdictNoReply
)

// String renders the verdict for logs.
func (v Verdict) String() string {
	switch v {
	case VerdictOK:
		return "ok"
	case VerdictTurnFailed:
		return "turn.failed"
	case VerdictNoThread:
		return "no thread.started"
	case VerdictIncomplete:
		return "no turn.completed"
	case VerdictNoReply:
		return "no final agent message"
	default:
		return "unknown"
	}
}

// Verdict decides whether the event stream describes a successful turn.
//
// Ordering matters. turn.failed outranks everything, because Codex can emit a
// partial agent message and then fail. A missing thread id outranks an
// incomplete turn, because without it the session cannot be resumed and the
// operator needs to know that specifically.
func (a *Accumulator) Verdict() Verdict {
	switch {
	case a.Failed:
		return VerdictTurnFailed
	case a.ThreadID == "":
		return VerdictNoThread
	case !a.Completed:
		return VerdictIncomplete
	case strings.TrimSpace(a.Reply) == "":
		return VerdictNoReply
	default:
		return VerdictOK
	}
}

// Reason builds a short diagnostic string for logs and for the error field of a
// turn row. It never includes raw JSON.
func (a *Accumulator) Reason() string {
	parts := make([]string, 0, 3)
	if len(a.ErrorNotes) > 0 {
		// The last note is usually the real reason; the earlier ones are
		// reconnect chatter.
		parts = append(parts, a.ErrorNotes[len(a.ErrorNotes)-1])
	}
	if a.MalformedLines > 0 {
		parts = append(parts, fmt.Sprintf("%d malformed JSONL line(s)", a.MalformedLines))
	}
	if a.OversizedLines > 0 {
		parts = append(parts, fmt.Sprintf("%d oversized JSONL line(s) skipped", a.OversizedLines))
	}
	if len(parts) == 0 {
		return a.Verdict().String()
	}
	return strings.Join(parts, "; ")
}

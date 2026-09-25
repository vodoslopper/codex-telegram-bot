// Package redact keeps secrets and noise out of logs and out of Telegram.
//
// Two different audiences need two different treatments. Logs must never carry
// the bot token, an API key or a whole user message. Telegram must never carry
// raw Codex JSON, hidden reasoning, or a credential that happened to appear in
// a stderr line. Both are handled here so the rules live in one place and are
// unit-testable, instead of being scattered across call sites.
package redact

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// secretPatterns match things that must not leave the process: OpenAI-style
// keys, bearer tokens, "key=value" credentials, and userinfo in URLs.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-[A-Za-z0-9_\-]{6,}`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{10,}\.[A-Za-z0-9_\-]{6,}`), // JWT
	regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._\-+/=]{8,}`),
	regexp.MustCompile(`(?i)\b(token|api[_-]?key|apikey|secret|password|passwd|authorization|access[_-]?token|refresh[_-]?token)(["']?\s*[:=]\s*)(\S+)`),
	regexp.MustCompile(`\bhttps?://[^\s/@:]+:[^\s/@]+@`),
	regexp.MustCompile(`(?i)\bCODEX_API_KEY=\S+`),
	regexp.MustCompile(`(?i)\bOPENAI_API_KEY=\S+`),
}

const replacement = "[redacted]"

// Secrets masks credential-looking substrings in s. It is applied to everything
// derived from a child process (stderr tails, error strings) before that text is
// logged or sent to Telegram.
func Secrets(s string) string {
	out := s
	for _, re := range secretPatterns {
		out = re.ReplaceAllString(out, replacement)
	}
	return out
}

// Token masks a Telegram bot token for logging: enough to tell two tokens
// apart, not enough to use either.
func Token(tok string) string {
	if tok == "" {
		return "<empty>"
	}
	if i := strings.IndexByte(tok, ':'); i > 0 {
		return tok[:i] + ":…masked…"
	}
	return "…masked…"
}

// Preview returns a short, single-line excerpt of a user message for debug
// logging. It is only called when the operator has explicitly enabled prompt
// logging; the default path logs lengths, never text.
func Preview(s string, maxRunes int) string {
	if maxRunes <= 0 {
		maxRunes = 40
	}
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, s)
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	trunc := make([]rune, 0, maxRunes)
	for _, r := range s {
		if len(trunc) == maxRunes {
			break
		}
		trunc = append(trunc, r)
	}
	return string(trunc) + "…"
}

// Tail returns the last maxBytes of s, prefixed with a marker when something was
// dropped. Codex writes its failure reason at the end of stderr, so the tail is
// the useful part; keeping the head instead would show only startup noise.
//
// The cut is made at a rune boundary and the result is secret-masked.
func Tail(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	s = strings.TrimSpace(s)
	if len(s) <= maxBytes {
		return Secrets(s)
	}
	cut := s[len(s)-maxBytes:]
	// Drop a leading partial rune.
	for len(cut) > 0 && !utf8.RuneStart(cut[0]) {
		cut = cut[1:]
	}
	return Secrets("…[truncated]…" + cut)
}

// OneLine flattens s for inclusion in a single log field.
func OneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

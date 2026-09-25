package redact

import (
	"strings"
	"testing"
)

func TestSecretsMasksCredentials(t *testing.T) {
	cases := []struct{ in, mustNotContain string }{
		{"key sk-AbCdEf1234567890_x-y here", "sk-AbCdEf1234567890"},
		{"Authorization: Bearer eyJhbGci.abcdefgh rest", "eyJhbGci.abcdefgh"},
		{"token=abc123secretvalue done", "abc123secretvalue"},
		{"api_key: \"SUPERSECRETVALUE\"", "SUPERSECRETVALUE"},
		{"password=hunter2hunter2", "hunter2hunter2"},
		{"url https://user:hunter2secret@api.example.com/x", "hunter2secret"},
		{"OPENAI_API_KEY=sk-live-abcdef123456", "sk-live-abcdef123456"},
		{"CODEX_API_KEY=whateveritis123", "whateveritis123"},
	}
	for _, c := range cases {
		got := Secrets(c.in)
		if strings.Contains(got, c.mustNotContain) {
			t.Errorf("Secrets(%q) = %q still contains %q", c.in, got, c.mustNotContain)
		}
		if !strings.Contains(got, replacement) {
			t.Errorf("Secrets(%q) = %q does not say anything was redacted", c.in, got)
		}
	}
}

func TestSecretsLeavesOrdinaryTextAlone(t *testing.T) {
	for _, in := range []string{
		"",
		"Codex exited with code 1",
		"no rollout found for thread id 0199a213-81c0-7800-8aa1-bbab2a035a53",
		"unexpected status 401 Unauthorized, url: wss://api.openai.com/v1/responses",
	} {
		if got := Secrets(in); got != in {
			t.Errorf("Secrets(%q) = %q, want it unchanged", in, got)
		}
	}
}

func TestTokenIsMasked(t *testing.T) {
	if got := Token("123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11"); got != "123456:…masked…" {
		t.Errorf("Token = %q", got)
	}
	if strings.Contains(Token("123456:ABC-DEF1234ghIkl-zyx57W2v1u123ew11"), "ABC-DEF") {
		t.Error("Token leaked the secret part")
	}
	if got := Token(""); got != "<empty>" {
		t.Errorf("Token(\"\") = %q", got)
	}
	if got := Token("noseparator"); !strings.Contains(got, "masked") {
		t.Errorf("Token(\"noseparator\") = %q", got)
	}
}

func TestTailKeepsTheEndAndIsBounded(t *testing.T) {
	in := strings.Repeat("a", 100) + "THE_REASON"
	got := Tail(in, 20)
	if !strings.Contains(got, "THE_REASON") {
		t.Errorf("Tail dropped the reason: %q", got)
	}
	if !strings.Contains(got, "truncated") {
		t.Errorf("Tail did not mark the truncation: %q", got)
	}
	if len(got) > 20+len("…[truncated]…") {
		t.Errorf("Tail is %d bytes, over the bound", len(got))
	}
	if Tail("", 10) != "" {
		t.Error("Tail of an empty string should be empty")
	}
	if Tail("short", 0) != "" {
		t.Error("Tail with a zero bound should be empty")
	}
}

func TestTailMasksSecretsInStderr(t *testing.T) {
	got := Tail("failed with sk-SuperSecretKey1234567890 at the end", 200)
	if strings.Contains(got, "SuperSecretKey") {
		t.Errorf("Tail leaked a key: %q", got)
	}
}

func TestTailDoesNotSplitARune(t *testing.T) {
	in := strings.Repeat("é", 50) // two bytes each
	got := Tail(in, 11)           // an odd cut, landing mid-rune
	for _, r := range got {
		if r == 0xFFFD && !strings.Contains(in, "\uFFFD") {
			t.Fatalf("Tail produced a replacement character, so it cut inside a rune: %q", got)
		}
	}
}

func TestPreviewFlattensAndTruncates(t *testing.T) {
	if got := Preview("line one\nline\ttwo", 100); got != "line one line two" {
		t.Errorf("Preview = %q", got)
	}
	got := Preview(strings.Repeat("x", 100), 10)
	if len([]rune(got)) != 11 { // 10 runes plus the ellipsis
		t.Errorf("Preview length = %d runes, want 11: %q", len([]rune(got)), got)
	}
}

func TestOneLine(t *testing.T) {
	if got := OneLine("a\n\n  b\tc\n"); got != "a b c" {
		t.Errorf("OneLine = %q", got)
	}
}

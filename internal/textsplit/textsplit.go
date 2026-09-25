// Package textsplit cuts a string into Telegram-sized messages.
//
// Telegram's limit on message text is 4096 characters, and "characters" there
// means UTF-16 code units, not Unicode code points: an emoji outside the Basic
// Multilingual Plane costs two. Counting Go runes therefore under-counts and a
// 4096-rune chunk can still be rejected by the API. This package measures in
// UTF-16 code units and only ever cuts at a rune boundary, so a surrogate pair
// is never split in half.
//
// Break preference, best first: a blank line, then a line break, then a space,
// then a hard cut. Codex replies are Markdown-ish prose with paragraphs and
// code blocks, so keeping paragraphs intact matters more than perfectly even
// chunk sizes.
package textsplit

import (
	"strings"
	"unicode/utf16"
)

// MaxText is Telegram's hard limit on message text, in UTF-16 code units.
const MaxText = 4096

// Units returns the length of s in UTF-16 code units.
func Units(s string) int {
	n := 0
	for _, r := range s {
		n += utf16.RuneLen(r)
	}
	return n
}

// Split cuts s into chunks of at most limit UTF-16 code units.
//
// It returns a single-element slice when s already fits, and never returns an
// empty chunk unless s itself is empty. limit is clamped to at least 1 so a
// misconfiguration degrades into many tiny messages instead of an infinite loop
// or a panic.
//
// One invariant has an exception worth stating: a chunk can exceed limit only
// when a *single rune* already does, because dropping content to fit would be
// worse than overshooting. At Telegram's 4096-unit limit that cannot happen —
// the widest rune costs 2 units — so it is only reachable with a limit of 1 in a
// test.
//
// Whitespace at a cut point is dropped: a chunk never ends with a newline and
// never starts with one. Nothing else is removed, so code blocks keep their
// indentation and blank lines.
func Split(s string, limit int) []string {
	if limit < 1 {
		limit = 1
	}
	if Units(s) <= limit {
		return []string{s}
	}

	var out []string
	rest := s
	for len(rest) > 0 {
		// take is the largest prefix of rest that fits, cut at a rune boundary.
		take := fittingPrefix(rest, limit)
		if take >= len(rest) {
			// The remainder fits: emit it verbatim and stop.
			out = append(out, rest)
			break
		}
		// cut is always >= 1, so every iteration consumes input.
		cut := bestBreak(rest[:take])
		chunk := strings.TrimRight(rest[:cut], cutset)
		rest = strings.TrimLeft(rest[cut:], cutset)
		if chunk != "" {
			out = append(out, chunk)
		}
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}

// cutset is the whitespace dropped at a cut point.
const cutset = " \t\r\n"

// fittingPrefix returns the byte length of the longest prefix of s whose UTF-16
// length is <= limit. The result is always a rune boundary, and always > 0 when
// s is non-empty, because a single rune costs at most 2 units and limit >= 1:
// if even the first rune does not fit we still take it, and the caller's hard
// cut keeps the loop moving.
func fittingPrefix(s string, limit int) int {
	units := 0
	for i, r := range s {
		w := utf16.RuneLen(r)
		if units+w > limit {
			if i == 0 {
				// A single rune wider than the limit cannot happen for
				// limit >= 2, but stay safe rather than loop forever.
				return len(string(r))
			}
			return i
		}
		units += w
	}
	return len(s)
}

// bestBreak picks where to cut inside window, which is known to fit. It prefers
// the latest paragraph break, then the latest line break, then the latest space,
// and hard-cuts at the end of the window when there is none. With no break at
// all the window end is already a rune boundary, so the result is always safe.
//
// A break that would throw away most of the window is ignored: cutting a 4096
// unit message after two characters because it happens to start with a blank
// line would waste a message and look broken. Short windows are exempt, since
// with a small limit any break is a good break.
//
// The candidates are tried latest-first and each is at least as late as the one
// before it, so rejecting an early paragraph break still leaves the later line
// and space breaks to choose from.
func bestBreak(window string) int {
	if i := strings.LastIndex(window, "\n\n"); i >= 0 && worthCutting(i+2, window) {
		return i + 2
	}
	if i := strings.LastIndex(window, "\n"); i >= 0 && worthCutting(i+1, window) {
		return i + 1
	}
	if i := strings.LastIndex(window, " "); i >= 0 && worthCutting(i+1, window) {
		return i + 1
	}
	return len(window)
}

// wasteFloor is the window size below which any break position is acceptable.
const wasteFloor = 64

// worthCutting reports whether cutting at n keeps enough of the window to be
// worth sending as its own message. n is always >= 1 here.
func worthCutting(n int, window string) bool {
	if n <= 0 {
		return false
	}
	if len(window) <= wasteFloor {
		return true
	}
	// Keep at least a quarter of the window.
	return n*4 >= len(window)
}

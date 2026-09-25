package textsplit

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestUnitsCountsUTF16(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"hello", 5},
		{"привет", 6},               // BMP: one unit per rune
		{"\U0001F600", 2},           // astral: a surrogate pair
		{"a\U0001F600b", 4},         // 1 + 2 + 1
		{"\U0001F1FA\U0001F1F8", 4}, // two flag emoji, two pairs
	}
	for _, c := range cases {
		if got := Units(c.in); got != c.want {
			t.Errorf("Units(%q) = %d, want %d", c.in, got, c.want)
		}
		if got := len([]rune(c.in)); got > c.want {
			t.Errorf("Units(%q) = %d is below the rune count %d, which is impossible", c.in, c.want, got)
		}
	}
}

func TestSplitShortTextIsOneChunk(t *testing.T) {
	for _, in := range []string{"", "hi", strings.Repeat("x", MaxText)} {
		got := Split(in, MaxText)
		if len(got) != 1 || got[0] != in {
			t.Errorf("Split(%d chars) = %d chunks, want the input back unchanged", len(in), len(got))
		}
	}
}

func TestSplitNeverExceedsLimit(t *testing.T) {
	// A mix of ASCII, BMP and astral characters: the case where counting runes
	// instead of UTF-16 units would silently produce oversized messages.
	var sb strings.Builder
	for i := 0; i < 500; i++ {
		sb.WriteString("word")
		switch i % 5 {
		case 0:
			sb.WriteString(" ")
		case 1:
			sb.WriteString("\n")
		case 2:
			sb.WriteString("\U0001F600") // 2 units
		case 3:
			sb.WriteString("é")
		default:
			sb.WriteString("\n\n")
		}
	}
	in := sb.String()

	for _, limit := range []int{1, 2, 3, 17, 100, 4096} {
		chunks := Split(in, limit)
		var rejoined strings.Builder
		for i, c := range chunks {
			if u := Units(c); u > limit {
				// The documented exception: a single rune wider than the limit
				// cannot be split without losing content.
				if !(limit == 1 && len([]rune(c)) == 1) {
					t.Fatalf("limit %d: chunk %d has %d UTF-16 units, over the limit: %q", limit, i, u, c)
				}
			}
			if !isValidUTF8NoLoneSurrogate(c) {
				t.Fatalf("limit %d: chunk %d is not valid UTF-8 / splits a surrogate pair: %q", limit, i, c)
			}
			rejoined.WriteString(c)
		}
		// Splitting may drop separating whitespace, so compare on the
		// whitespace-stripped form: no content may be lost or reordered.
		if squeeze(rejoined.String()) != squeeze(in) {
			t.Fatalf("limit %d: reassembled text differs from the input", limit)
		}
	}
}

func TestSplitPrefersParagraphAndLineBreaks(t *testing.T) {
	para := "aaaa\n\n" + strings.Repeat("b", 20)
	chunks := Split(para, 12)
	if len(chunks) < 2 {
		t.Fatalf("expected the text to be split, got %d chunk(s): %q", len(chunks), chunks)
	}
	if !strings.HasSuffix(chunks[0], "aaaa") {
		t.Errorf("first chunk should end at the paragraph break, got %q", chunks[0])
	}

	lines := "one\ntwo\nthree\nfour\nfive"
	chunks = Split(lines, 9)
	for _, c := range chunks {
		if strings.HasPrefix(c, "\n") || strings.HasSuffix(c, "\n") {
			t.Errorf("chunk carries a stray newline: %q", c)
		}
	}
}

func TestSplitHardCutsAnUnbreakableRun(t *testing.T) {
	in := strings.Repeat("x", 100)
	chunks := Split(in, 10)
	if len(chunks) != 10 {
		t.Fatalf("got %d chunks, want 10: %q", len(chunks), chunks)
	}
	for _, c := range chunks {
		if len(c) != 10 {
			t.Errorf("chunk %q is not 10 bytes", c)
		}
	}
}

func TestSplitDoesNotEmitEmptyChunks(t *testing.T) {
	for _, in := range []string{"\n\n\n\n", "   ", "\n" + strings.Repeat("y", 50)} {
		for _, c := range Split(in, 8) {
			if c == "" {
				t.Errorf("empty chunk for input %q", in)
			}
		}
	}
}

func TestSplitClampsANonsenseLimit(t *testing.T) {
	// A misconfigured limit must degrade into many small messages, not panic or
	// loop forever.
	got := Split("abcdef", 0)
	if len(got) == 0 {
		t.Fatal("limit 0 produced no chunks")
	}
	for _, c := range got {
		if Units(c) > 1 {
			t.Errorf("limit 0 produced a chunk of %d units: %q", Units(c), c)
		}
	}
}

func TestSplitAstralnputIsNeverHalved(t *testing.T) {
	// One emoji per line, limit 3 units: an emoji costs 2, so each chunk can
	// hold exactly one emoji plus at most one ASCII character. A bug that cut on
	// byte or rune counts would produce an invalid sequence.
	in := strings.Repeat("\U0001F600\n", 20)
	for _, c := range Split(in, 3) {
		if !isValidUTF8NoLoneSurrogate(c) {
			t.Fatalf("chunk splits a surrogate pair: %q", c)
		}
		if Units(c) > 3 {
			t.Fatalf("chunk has %d units, limit is 3", Units(c))
		}
	}
}

// isValidUTF8NoLoneSurrogate reports whether s is valid UTF-8.
//
// A split surrogate pair cannot be represented in UTF-8 at all: cutting a Go
// string at a non-rune boundary leaves a byte sequence utf8.Valid rejects. So
// this one check catches exactly the failure mode a naive byte- or rune-counting
// splitter would produce.
func isValidUTF8NoLoneSurrogate(s string) bool { return utf8.ValidString(s) }

// squeeze removes all whitespace so a comparison ignores the spaces Split is
// allowed to drop at a cut point.
func squeeze(s string) string {
	return strings.Join(strings.Fields(s), "")
}

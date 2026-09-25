package sessid

import (
	"strings"
	"testing"
)

func TestNewShape(t *testing.T) {
	for i := 0; i < 1000; i++ {
		id := New()
		if len(id) != IDLen {
			t.Fatalf("New() = %q, length %d, want %d", id, len(id), IDLen)
		}
		if !strings.HasPrefix(id, Prefix) {
			t.Fatalf("New() = %q does not start with %q", id, Prefix)
		}
		if !Valid(id) {
			t.Fatalf("New() = %q is rejected by Valid", id)
		}
	}
}

func TestNewIsRandom(t *testing.T) {
	seen := make(map[string]struct{}, 5000)
	for i := 0; i < 5000; i++ {
		id := New()
		if _, dup := seen[id]; dup {
			t.Fatalf("New() repeated %q within 5000 draws", id)
		}
		seen[id] = struct{}{}
	}
}

func TestIDCanNeverBeAUUID(t *testing.T) {
	// The whole point of the "s" prefix: an id this bot hands out must not be
	// something `codex exec resume` could interpret as a thread.
	for i := 0; i < 1000; i++ {
		id := New()
		if isUUIDShape(id) {
			t.Fatalf("New() = %q has a UUID shape", id)
		}
		// 's' is not a hexadecimal digit, so no id can parse as a UUID.
		if !strings.Contains(id, "s") {
			t.Fatalf("New() = %q does not contain the prefix", id)
		}
	}
}

func TestIDCanNeverLookLikeAFlag(t *testing.T) {
	for i := 0; i < 1000; i++ {
		id := New()
		if strings.HasPrefix(id, "-") || strings.Contains(id, "-") {
			t.Fatalf("New() = %q contains a dash", id)
		}
		if strings.ContainsAny(id, " \t\n\"'`$\\") {
			t.Fatalf("New() = %q contains a shell or whitespace metacharacter", id)
		}
	}
}

func TestValidRejects(t *testing.T) {
	bad := []string{
		"",
		"s",
		"s1234",                                // too short
		"s1234567",                             // too long
		"x7k3qm2",                              // wrong prefix
		"S7K3QM2",                              // upper case
		"s0123456",                             // contains excluded 0 and 1
		"sio7k3m",                              // contains excluded i and o
		"s7k3qm-",                              // a dash
		"s7k3qm ",                              // a space
		"0199a213-81c0-7800-8aa1-bbab2a035a53", // a Codex thread UUID
		"--sandbox",                            // a flag
		"s7k3qm2;rm",                           // shell metacharacters
	}
	for _, id := range bad {
		if Valid(id) {
			t.Errorf("Valid(%q) = true, want false", id)
		}
	}
}

func TestValidAccepts(t *testing.T) {
	for _, id := range []string{"s234567", "sabcdef", "szzzzzz", Prefix + "23456789"[:BodyLen]} {
		if !Valid(id) {
			t.Errorf("Valid(%q) = false, want true", id)
		}
	}
}

func TestAlphabetHasNoAmbiguousCharacters(t *testing.T) {
	for _, c := range "01iloO" {
		if strings.ContainsRune(alphabet, c) {
			t.Errorf("alphabet contains the ambiguous character %q", c)
		}
	}
	if len(alphabet) != 31 {
		t.Errorf("alphabet has %d characters, want 31", len(alphabet))
	}
}

func isUUIDShape(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, r := range s {
		hex := (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')
		switch i {
		case 8, 13, 18, 23:
			if r != '-' {
				return false
			}
		default:
			if !hex {
				return false
			}
		}
	}
	return true
}

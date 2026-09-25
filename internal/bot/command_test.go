package bot

import (
	"strings"
	"testing"
)

func TestParseCommand(t *testing.T) {
	cases := []struct {
		name        string
		in          string
		botUsername string
		want        *Command
	}{
		{
			name: "plain",
			in:   "/help",
			want: &Command{Name: "help"},
		},
		{
			name: "lowercased",
			in:   "/NEW",
			want: &Command{Name: "new"},
		},
		{
			name: "leading whitespace",
			in:   "   /sessions all",
			want: &Command{Name: "sessions", Args: []string{"all"}, Rest: "all"},
		},
		{
			name: "one argument",
			in:   "/use s7k3qm2",
			want: &Command{Name: "use", Args: []string{"s7k3qm2"}, Rest: "s7k3qm2"},
		},
		{
			name: "a name with spaces keeps them in Rest",
			in:   "/rename s7k3qm2 packaging work",
			want: &Command{Name: "rename", Args: []string{"s7k3qm2", "packaging", "work"}, Rest: "s7k3qm2 packaging work"},
		},
		{
			name:        "addressed to us",
			in:          "/new@codex_test_bot thing",
			botUsername: "codex_test_bot",
			want:        &Command{Name: "new", Args: []string{"thing"}, Rest: "thing"},
		},
		{
			name:        "addressed to us, different case",
			in:          "/new@Codex_Test_Bot",
			botUsername: "codex_test_bot",
			want:        &Command{Name: "new"},
		},
		{
			name:        "addressed to another bot",
			in:          "/new@otherbot thing",
			botUsername: "codex_test_bot",
			want:        &Command{Name: "new", OtherBot: true},
		},
		{
			name: "a mention with no known username is treated as ours",
			in:   "/new@whatever thing",
			want: &Command{Name: "new", Args: []string{"thing"}, Rest: "thing"},
		},
		{
			name: "newlines inside the argument block",
			in:   "/new my\nsession",
			want: &Command{Name: "new", Args: []string{"my", "session"}, Rest: "my\nsession"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ParseCommand(c.in, c.botUsername)
			if c.want == nil {
				if got != nil {
					t.Fatalf("ParseCommand(%q) = %+v, want nil", c.in, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("ParseCommand(%q) = nil, want %+v", c.in, c.want)
			}
			if got.Name != c.want.Name {
				t.Errorf("Name = %q, want %q", got.Name, c.want.Name)
			}
			if got.OtherBot != c.want.OtherBot {
				t.Errorf("OtherBot = %v, want %v", got.OtherBot, c.want.OtherBot)
			}
			if got.Rest != c.want.Rest {
				t.Errorf("Rest = %q, want %q", got.Rest, c.want.Rest)
			}
			if strings.Join(got.Args, "|") != strings.Join(c.want.Args, "|") {
				t.Errorf("Args = %q, want %q", got.Args, c.want.Args)
			}
		})
	}
}

func TestParseCommandRejectsNonCommands(t *testing.T) {
	for _, in := range []string{
		"",
		"hello",
		"please /help me", // the slash is not at the start
		"  summarize this",
		"/",  // nothing after the slash
		"/@", // no name
	} {
		if got := ParseCommand(in, "bot"); got != nil {
			t.Errorf("ParseCommand(%q) = %+v, want nil", in, got)
		}
	}
}

func TestFirstField(t *testing.T) {
	cases := []struct{ in, field, rest string }{
		{"a b c", "a", "b c"},
		{"  a   b", "a", "  b"},
		{"only", "only", ""},
		{"", "", ""},
		{"a\nb", "a", "b"},
	}
	for _, c := range cases {
		f, r := firstField(c.in)
		if f != c.field || r != c.rest {
			t.Errorf("firstField(%q) = (%q, %q), want (%q, %q)", c.in, f, r, c.field, c.rest)
		}
	}
}

func TestBadIDTextExplainsAUUID(t *testing.T) {
	// The most likely mistake is pasting a Codex thread UUID where a bot session
	// id belongs. The answer should say so rather than "invalid".
	got := badIDText("0199a213-81c0-7800-8aa1-bbab2a035a53")
	if !strings.Contains(got, "UUID") {
		t.Errorf("badIDText for a UUID = %q, want it to name the confusion", got)
	}
	short := badIDText("nope")
	if !strings.Contains(short, "s7k3qm") {
		t.Errorf("badIDText for junk = %q, want an example of the right shape", short)
	}
}

func TestScopeString(t *testing.T) {
	if got := (Scope{ChatID: 5}).String(); got != "5" {
		t.Errorf("Scope.String() = %q", got)
	}
	if got := (Scope{ChatID: 5, ThreadID: 7}).String(); got != "5/7" {
		t.Errorf("Scope.String() = %q", got)
	}
}

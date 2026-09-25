package bot

import (
	"strconv"
	"strings"
)

// Command is a parsed "/name args" message.
type Command struct {
	// Name is lower-case and has no leading slash or @mention.
	Name string
	// OtherBot is true when the command was addressed to a different bot, e.g.
	// "/help@someotherbot". Such a command is ignored entirely.
	OtherBot bool
	// Args is the remainder split on whitespace.
	Args []string
	// Rest is the remainder with surrounding space trimmed but inner spacing
	// intact, which is what /rename needs to accept a multi-word name.
	Rest string
}

// ParseCommand parses a message as a command, or returns nil if it is not one.
//
// botUsername is the bot's own username without the leading "@", used to accept
// "/new@thisbot" and ignore "/new@otherbot". It may be empty when getMe has not
// run, in which case any mention is treated as ours — a private chat cannot
// contain another bot's commands anyway.
func ParseCommand(text, botUsername string) *Command {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "/") {
		return nil
	}
	head, rest := firstField(trimmed)
	head = strings.TrimPrefix(head, "/")
	if head == "" {
		return nil
	}

	name := head
	cmd := &Command{Rest: strings.TrimSpace(rest)}
	if at := strings.IndexByte(head, '@'); at >= 0 {
		name = head[:at]
		mention := head[at+1:]
		if name == "" {
			return nil
		}
		if botUsername != "" && !strings.EqualFold(mention, botUsername) {
			// Addressed to a different bot: report the name so the caller can
			// log it, and carry nothing else, because nothing else will be used.
			return &Command{Name: strings.ToLower(name), OtherBot: true}
		}
	}
	cmd.Name = strings.ToLower(name)
	cmd.Args = strings.Fields(rest)
	return cmd
}

// firstField splits off the first whitespace-delimited token and returns it with
// the remainder. It is used instead of strings.Fields when the remainder must
// keep its internal spacing.
func firstField(s string) (field, remainder string) {
	s = strings.TrimLeft(s, " \t\n\r")
	i := strings.IndexAny(s, " \t\n\r")
	if i < 0 {
		return s, ""
	}
	return s[:i], s[i+1:]
}

// itoa is a small helper so the package does not import strconv everywhere.
func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// Package sessid mints the short session ids the bot shows its users.
//
// A bot session id is deliberately not a Codex thread id. Codex threads are
// UUIDs ("01a0d916-0c41-76b2-ae57-cb11a3f51f76"), which are long, easy to
// mistype in a phone chat, and — critically — meaningful to the Codex CLI. If
// the bot handed out identifiers that a user could also type into
// `codex exec resume`, a guessed or copied value could address somebody else's
// conversation. These ids start with "s", which is not a hexadecimal digit, so
// they can never be parsed as a UUID, and they contain no "-", so they can
// never be mistaken for a command-line flag.
//
// The alphabet drops 0, 1, i, l and o so an id read aloud or copied by hand
// stays unambiguous.
package sessid

import (
	"crypto/rand"
	"math/big"
)

// alphabet has 31 characters, none of them visually ambiguous.
const alphabet = "23456789abcdefghjkmnpqrstuvwxyz"

// Prefix marks every bot session id.
const Prefix = "s"

// BodyLen is the number of random characters after the prefix: 31^6 is about
// 8.9e8, so collisions stay rare for a single-user bot and the store retries on
// the odd one.
const BodyLen = 6

// IDLen is the total length of a session id, prefix included.
const IDLen = 1 + BodyLen

// New returns a fresh random session id. It panics only if the system CSPRNG is
// unavailable, which Go treats as unrecoverable everywhere else too.
func New() string {
	max := big.NewInt(int64(len(alphabet)))
	b := make([]byte, 0, IDLen)
	b = append(b, Prefix...)
	for i := 0; i < BodyLen; i++ {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			panic("sessid: crypto/rand failed: " + err.Error())
		}
		b = append(b, alphabet[n.Int64()])
	}
	return string(b)
}

// Valid reports whether s has exactly the shape New produces. Command arguments
// are checked with this before they are used in a query, so a stray UUID or a
// value with shell or flag metacharacters is rejected at the door.
func Valid(s string) bool {
	if len(s) != IDLen || s[0] != Prefix[0] {
		return false
	}
	for i := 1; i < len(s); i++ {
		found := false
		for j := 0; j < len(alphabet); j++ {
			if s[i] == alphabet[j] {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

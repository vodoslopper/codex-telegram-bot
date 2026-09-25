// Package lockfile holds an exclusive advisory lock on a file.
//
// Telegram delivers each update exactly once to whichever client polls with the
// lowest offset, and two concurrent pollers would each advance the shared
// offset and silently steal updates from the other. The bot therefore refuses to
// start when another live process already holds BOT_STATE_DIR/poller.lock.
//
// flock(2) is used rather than O_EXCL creation because the kernel releases it
// when the process dies, however it dies. A stale PID file would wedge the
// service until somebody deleted it by hand.
package lockfile

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// ErrHeld means another live process owns the lock.
var ErrHeld = errors.New("another process holds the lock")

// Lock is a held file lock. Release it with Close.
type Lock struct {
	path string
	f    *os.File
}

// Acquire opens path (creating it 0600 if needed) and takes an exclusive
// non-blocking lock on it. It returns ErrHeld, wrapped with the path, when the
// lock is already taken.
func Acquire(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%s: %w", path, ErrHeld)
		}
		return nil, fmt.Errorf("flock %s: %w", path, err)
	}
	// Record the owner for a human poking at the directory. Best effort: a
	// failure to truncate or write must not lose the lock we already hold.
	_ = f.Truncate(0)
	_, _ = f.WriteString(fmt.Sprintf("pid=%d\n", os.Getpid()))
	return &Lock{path: path, f: f}, nil
}

// Path is the file backing the lock. It is safe on a nil Lock, because the
// natural place to call it is an error path where Acquire returned none.
func (l *Lock) Path() string {
	if l == nil {
		return ""
	}
	return l.path
}

// Close releases the lock and closes the file. The file itself is left in place.
func (l *Lock) Close() error {
	if l == nil || l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	// Ignoring the flock error: the close below drops the descriptor, which
	// releases the lock either way.
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return f.Close()
}

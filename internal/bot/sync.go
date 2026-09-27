package bot

import (
	"context"
	"sync"
	"time"
)

// Scope identifies where a conversation happens: a private chat, optionally one
// topic inside it, and the user who owns it.
//
// Telegram private chats can have topics (message_thread_id), and a user talking
// to the bot in two topics is having two conversations. Selections and in-flight
// turn tracking are therefore keyed on all three parts, not just the user.
type Scope struct {
	ChatID   int64
	ThreadID int64 // 0 when the chat has no topics
	UserID   int64
}

// key is the map key form. Scope is comparable, so it is used directly; this
// exists to make the intent obvious at call sites that build string keys.
func (s Scope) String() string {
	if s.ThreadID == 0 {
		return itoa(s.ChatID)
	}
	return itoa(s.ChatID) + "/" + itoa(s.ThreadID)
}

// --- in-flight turns -------------------------------------------------------

// inflightTurn is a running or queued turn that /stop can cancel.
type inflightTurn struct {
	mu        sync.Mutex
	cancel    context.CancelFunc
	sessionID string
	turnID    int64
	started   time.Time
	running   bool
}

func (t *inflightTurn) setSession(id string) {
	t.mu.Lock()
	t.sessionID = id
	t.mu.Unlock()
}

func (t *inflightTurn) setTurn(id int64) {
	t.mu.Lock()
	t.turnID = id
	t.mu.Unlock()
}

func (t *inflightTurn) ids() (string, int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionID, t.turnID
}

func (t *inflightTurn) setRunning() {
	t.mu.Lock()
	t.running = true
	t.mu.Unlock()
}

func (t *inflightTurn) state() (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.sessionID, t.running
}

// inflightRegistry maps a scope to its current turn.
//
// A scope can have one running turn and several queued turns. /stop targets the
// oldest one; it cannot take the scope lock because the active turn holds it.
type inflightRegistry struct {
	mu sync.Mutex
	m  map[Scope][]*inflightTurn
}

func newInflightRegistry() *inflightRegistry {
	return &inflightRegistry{m: make(map[Scope][]*inflightTurn)}
}

// register adds a turn in arrival order, including turns waiting for the lock.
func (r *inflightRegistry) register(s Scope, t *inflightTurn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[s] = append(r.m[s], t)
}

// clear removes exactly this turn without disturbing any other queued turn.
func (r *inflightRegistry) clear(s Scope, t *inflightTurn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	entries := r.m[s]
	for i, entry := range entries {
		if entry == t {
			entries = append(entries[:i], entries[i+1:]...)
			if len(entries) == 0 {
				delete(r.m, s)
			} else {
				r.m[s] = entries
			}
			return
		}
	}
}

// get returns the current turn for a scope, or nil.
func (r *inflightRegistry) get(s Scope) *inflightTurn {
	r.mu.Lock()
	defer r.mu.Unlock()
	if entries := r.m[s]; len(entries) > 0 {
		return entries[0]
	}
	return nil
}

// --- keyed locks -----------------------------------------------------------

// lockEntry is one named mutex, reference-counted so the map does not grow
// without bound.
//
// The channel is the mutex, in its "token" form: it is buffered at 1, acquiring
// means sending a token into it, and releasing means taking one back out. So a
// token *present* means locked, and an empty channel means free. That is the
// opposite of the more familiar "a token in the channel means available" idiom,
// and worth stating because it is easy to invert.
type lockEntry struct {
	ch chan struct{}
	n  int // waiters plus the holder, so the map entry can be reclaimed
}

// keyedLocks serialises work under a name.
//
// Two different names are used: a scope key, so one chat's turns run one after
// another in arrival order, and a workspace key, so turns sharing a working tree
// never overlap. Channel handoff is FIFO in the Go runtime, which is what makes
// "arrival order" true rather than merely likely.
type keyedLocks struct {
	mu sync.Mutex
	m  map[string]*lockEntry
}

func newKeyedLocks() *keyedLocks {
	return &keyedLocks{m: make(map[string]*lockEntry)}
}

// Acquire takes the lock for key, waiting up to the context's deadline.
//
// The returned release function must be called exactly once. A context that
// ends first returns its error and no release function, so the caller can tell
// "I waited too long" from "I have the lock".
func (k *keyedLocks) Acquire(ctx context.Context, key string) (func(), error) {
	k.mu.Lock()
	e, ok := k.m[key]
	if !ok {
		e = &lockEntry{ch: make(chan struct{}, 1)}
		k.m[key] = e
	}
	e.n++
	k.mu.Unlock()

	select {
	case e.ch <- struct{}{}:
	case <-ctx.Done():
		k.forget(key, e)
		return nil, ctx.Err()
	}

	var once sync.Once
	return func() {
		once.Do(func() {
			<-e.ch
			k.forget(key, e)
		})
	}, nil
}

// forget drops a waiter, and deletes the entry once nobody cares about it.
func (k *keyedLocks) forget(key string, e *lockEntry) {
	k.mu.Lock()
	defer k.mu.Unlock()
	e.n--
	if e.n <= 0 && len(e.ch) == 0 {
		if cur, ok := k.m[key]; ok && cur == e {
			delete(k.m, key)
		}
	}
}

// Held reports whether key is currently locked.
//
// Best effort, and only ever used for status reporting and tests: channel
// operations are not synchronised with k.mu, so this is a snapshot rather than a
// promise. It must not try to acquire and release the lock to find out — that
// would both race with a real acquirer and, since the channel is buffered at one,
// block forever the moment somebody else took it in between.
func (k *keyedLocks) Held(key string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	e, ok := k.m[key]
	if !ok {
		return false // nobody holds it and nobody is waiting for it
	}
	return len(e.ch) == 1
}

// waiters reports how many callers are queued on key, for tests and logs.
func (k *keyedLocks) waiters(key string) int {
	k.mu.Lock()
	defer k.mu.Unlock()
	if e, ok := k.m[key]; ok {
		return e.n
	}
	return 0
}

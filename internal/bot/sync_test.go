package bot

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestKeyedLocksAreMutuallyExclusive(t *testing.T) {
	k := newKeyedLocks()
	const goroutines = 12
	const hold = 20 * time.Millisecond

	var (
		wg      sync.WaitGroup
		inside  atomic.Int32
		maxSeen atomic.Int32
	)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := k.Acquire(context.Background(), "workspace")
			if err != nil {
				t.Errorf("Acquire: %v", err)
				return
			}
			n := inside.Add(1)
			for {
				old := maxSeen.Load()
				if n <= old || maxSeen.CompareAndSwap(old, n) {
					break
				}
			}
			time.Sleep(hold)
			inside.Add(-1)
			release()
		}()
	}
	wg.Wait()

	if got := maxSeen.Load(); got != 1 {
		t.Errorf("%d goroutines held the same key at once; turns sharing a worktree must never overlap", got)
	}
	if k.Held("workspace") {
		t.Error("the key is still held after every acquirer released it")
	}
	if n := k.waiters("workspace"); n != 0 {
		t.Errorf("%d waiter(s) are still counted after they all finished", n)
	}
}

func TestKeyedLocksAreIndependentPerKey(t *testing.T) {
	k := newKeyedLocks()
	releaseA, err := k.Acquire(context.Background(), "a")
	if err != nil {
		t.Fatalf("Acquire(a): %v", err)
	}
	defer releaseA()

	if !k.Held("a") {
		t.Error("Held(a) = false while a is held")
	}
	// A different key must not be blocked by "a": that is what lets two chats
	// with two different workspaces run at once.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	releaseB, err := k.Acquire(ctx, "b")
	if err != nil {
		t.Fatalf("Acquire(b) was blocked by a: %v", err)
	}
	releaseB()
	if k.Held("b") {
		t.Error("Held(b) = true after releasing it")
	}
}

func TestKeyedLocksAcquireIsCancellable(t *testing.T) {
	k := newKeyedLocks()
	release, err := k.Acquire(context.Background(), "busy")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := k.Acquire(ctx, "busy"); err == nil {
		t.Fatal("Acquire succeeded on a key somebody else holds")
	} else if err != context.DeadlineExceeded {
		t.Errorf("Acquire returned %v, want the context error so a caller can tell a timeout from a cancel", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("a cancellable Acquire took %s to give up", elapsed)
	}
	// The abandoned waiter must not be counted any more, or the map entry would
	// never be reclaimed.
	if n := k.waiters("busy"); n != 1 {
		t.Errorf("waiters = %d, want only the holder", n)
	}
}

func TestKeyedLocksReleaseIsIdempotent(t *testing.T) {
	k := newKeyedLocks()
	release, err := k.Acquire(context.Background(), "once")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	release()
	release() // a double release must not free somebody else's lock
	release()

	if k.Held("once") {
		t.Error("the key is still held")
	}
	// And the key is genuinely available again, exactly once.
	again, err := k.Acquire(context.Background(), "once")
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	again()
}

func TestKeyedLocksHandOffInOrder(t *testing.T) {
	// Channel handoff in the Go runtime is FIFO, which is what makes "one chat's
	// messages run in arrival order" true rather than merely likely.
	k := newKeyedLocks()
	release, err := k.Acquire(context.Background(), "queue")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	const n = 6
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		order []int
		ready sync.WaitGroup
	)
	ready.Add(n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ready.Done()
			// Stagger slightly so "arrival order" is unambiguous.
			time.Sleep(time.Duration(i) * 15 * time.Millisecond)
			r, err := k.Acquire(context.Background(), "queue")
			if err != nil {
				t.Errorf("Acquire %d: %v", i, err)
				return
			}
			mu.Lock()
			order = append(order, i)
			mu.Unlock()
			r()
		}(i)
	}
	ready.Wait()
	time.Sleep(time.Duration(n) * 15 * time.Millisecond)
	release()
	wg.Wait()

	if len(order) != n {
		t.Fatalf("%d of %d goroutines got the lock", len(order), n)
	}
	for i, v := range order {
		if i != v {
			t.Errorf("the lock was handed off out of order: %v", order)
			break
		}
	}
}

func TestInflightRegistry(t *testing.T) {
	r := newInflightRegistry()
	scope := Scope{ChatID: 1, ThreadID: 2, UserID: 3}

	if r.get(scope) != nil {
		t.Error("an empty registry returned a turn")
	}
	cancelled := false
	entry := &inflightTurn{cancel: func() { cancelled = true }, sessionID: "s1", turnID: 7}
	r.register(scope, entry)

	if got := r.get(scope); got != entry {
		t.Errorf("get returned %+v, want the registered entry", got)
	}
	// A different topic in the same chat is a different conversation.
	if r.get(Scope{ChatID: 1, ThreadID: 9, UserID: 3}) != nil {
		t.Error("a different topic saw this scope's turn")
	}

	// A queued turn must not replace the active one as /stop's target.
	newer := &inflightTurn{cancel: func() {}, sessionID: "s2"}
	r.register(scope, newer)
	if got := r.get(scope); got != entry {
		t.Errorf("queued turn replaced active /stop target: %+v", got)
	}
	r.clear(scope, entry)
	if got := r.get(scope); got != newer {
		t.Errorf("a stale clear removed the current turn: %+v", got)
	}
	r.clear(scope, newer)
	if r.get(scope) != nil {
		t.Error("the entry survived a matching clear")
	}
	entry.cancel()
	if !cancelled {
		t.Error("the recorded cancel function does not cancel")
	}
}

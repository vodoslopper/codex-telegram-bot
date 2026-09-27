package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"codex-telegram-bot/migrations"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	if err := chmod0700(dir); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	s, err := Open(dir+"/bot.db", nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func chmod0700(dir string) error { return mkdirAll(dir, 0o700) }

func ctx() context.Context { return context.Background() }

func TestOpenMigratesAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/bot.db"

	first, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := first.CreateSession(ctx(), 1, "x", "/ws"); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopening must not re-run the migrations or lose the row.
	second, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()

	sessions, err := second.ListSessions(ctx(), 1, false)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Errorf("after reopening there are %d session(s), want 1", len(sessions))
	}

	var applied int
	if err := second.DB().QueryRowContext(ctx(), `SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	if applied != len(Migrations()) {
		t.Errorf("%d migrations recorded, want %d", applied, len(Migrations()))
	}
}

func TestOpenUpgradesOldUndeliveredReplyAutomatically(t *testing.T) {
	path := t.TempDir() + "/bot.db"
	db, err := sql.Open(DriverName, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx(), `CREATE TABLE schema_migrations (
		version TEXT PRIMARY KEY, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"001_init.sql", "002_model_settings.sql", "003_retained_media.sql"} {
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.ExecContext(ctx(), string(body)); err != nil {
			t.Fatalf("apply old migration %s: %v", name, err)
		}
		if _, err := db.ExecContext(ctx(), `INSERT INTO schema_migrations (version, applied_at)
			VALUES (?, ?)`, name, "2026-01-01T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	const selected = "2026-01-01T00:00:00Z"
	const started = "2026-01-01T00:01:00Z"
	if _, err := db.ExecContext(ctx(), `INSERT INTO sessions
		(id, owner_user_id, name, workspace, created_at, updated_at)
		VALUES ('s7k3qm', 111, 'old', '/ws', ?, ?)`, selected, selected); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx(), `INSERT INTO selections
		(chat_id, thread_id, user_id, session_id, updated_at)
		VALUES (111, 17, 111, 's7k3qm', ?)`, selected); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx(), `INSERT INTO processed_updates
		(update_id, user_id, chat_id, kind, created_at)
		VALUES (42, 111, 111, 'text', ?)`, started); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx(), `INSERT INTO turns
		(update_id, session_id, owner_user_id, status, reply, delivered, started_at, finished_at)
		VALUES (42, 's7k3qm', 111, 'completed', 'old answer', 0, ?, ?)`, started, started); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := Open(path, nil)
	if err != nil {
		t.Fatalf("open old database: %v", err)
	}
	defer upgraded.Close()
	pending, err := upgraded.PendingReplies(ctx(), 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || !pending[0].Legacy || !pending[0].TopicKnown ||
		pending[0].ThreadID != 17 || pending[0].Reply != "old answer" {
		t.Fatalf("old reply after automatic upgrade = %+v", pending)
	}
}

func TestDatabaseFileIsPrivate(t *testing.T) {
	dir := t.TempDir()
	if err := chmod0700(dir); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	s, err := Open(dir+"/bot.db", nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// Touch the file so it exists.
	if err := s.SetMeta(ctx(), "k", "v"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	perm, err := FileModeOf(dir + "/bot.db")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm&0o077 != 0 {
		t.Errorf("the database is group/world accessible (mode %o); it holds conversation replies", perm)
	}
}

func TestSessionOwnershipIsEnforcedInTheQuery(t *testing.T) {
	s := openStore(t)
	const alice, bob = int64(111), int64(222)

	sess, err := s.CreateSession(ctx(), alice, "alice work", "/ws")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Bob must not be able to read it, and must not learn that it exists: the
	// answer has to be indistinguishable from a typo'd id.
	if _, err := s.GetSession(ctx(), sess.ID, bob); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetSession as bob = %v, want ErrNotFound", err)
	}
	if err := s.RenameSession(ctx(), sess.ID, bob, "pwned"); !errors.Is(err, ErrNotFound) {
		t.Errorf("RenameSession as bob = %v, want ErrNotFound", err)
	}
	if err := s.SetArchived(ctx(), sess.ID, bob, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetArchived as bob = %v, want ErrNotFound", err)
	}
	if err := s.SetThreadID(ctx(), sess.ID, bob, testUUID(1)); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetThreadID as bob = %v, want ErrNotFound", err)
	}
	if err := s.SelectSession(ctx(), 2, 0, bob, sess.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("SelectSession as bob = %v, want ErrNotFound", err)
	}

	// Alice still sees her own row untouched.
	got, err := s.GetSession(ctx(), sess.ID, alice)
	if err != nil {
		t.Fatalf("GetSession as alice: %v", err)
	}
	if got.Name != "alice work" || got.Archived {
		t.Errorf("alice's session was modified by bob's attempts: %+v", got)
	}

	// And bob's listing does not contain it.
	list, err := s.ListSessions(ctx(), bob, true)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("bob can list %d of alice's sessions", len(list))
	}
}

func TestSessionIDsAreUniqueAndShaped(t *testing.T) {
	s := openStore(t)
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		sess, err := s.CreateSession(ctx(), 1, "", "/ws")
		if err != nil {
			t.Fatalf("CreateSession %d: %v", i, err)
		}
		if seen[sess.ID] {
			t.Fatalf("duplicate session id %q", sess.ID)
		}
		seen[sess.ID] = true
		if len(sess.ID) != 7 || sess.ID[0] != 's' {
			t.Fatalf("session id %q has an unexpected shape", sess.ID)
		}
	}
	n, err := s.CountSessions(ctx(), 1)
	if err != nil {
		t.Fatalf("CountSessions: %v", err)
	}
	if n != 200 {
		t.Errorf("CountSessions = %d, want 200", n)
	}
}

func TestCreateSessionValidatesItsArguments(t *testing.T) {
	s := openStore(t)
	if _, err := s.CreateSession(ctx(), 0, "x", "/ws"); err == nil {
		t.Error("CreateSession accepted owner 0")
	}
	if _, err := s.CreateSession(ctx(), -1, "x", "/ws"); err == nil {
		t.Error("CreateSession accepted a negative owner")
	}
	if _, err := s.CreateSession(ctx(), 1, "x", ""); err == nil {
		t.Error("CreateSession accepted an empty workspace")
	}
}

func TestRenameAndArchive(t *testing.T) {
	s := openStore(t)
	sess, err := s.CreateSession(ctx(), 7, "original", "/ws")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if err := s.RenameSession(ctx(), sess.ID, 7, "  renamed  "); err != nil {
		t.Fatalf("RenameSession: %v", err)
	}
	got, _ := s.GetSession(ctx(), sess.ID, 7)
	if got.Name != "renamed" {
		t.Errorf("Name = %q, want the trimmed %q", got.Name, "renamed")
	}
	if err := s.RenameSession(ctx(), sess.ID, 7, "   "); err == nil {
		t.Error("RenameSession accepted an empty name")
	}
	if err := s.RenameSession(ctx(), sess.ID, 7, strings.Repeat("x", 121)); err == nil {
		t.Error("RenameSession accepted a 121-character name")
	}

	// Archived sessions disappear from the default listing but not from `all`.
	if err := s.SetArchived(ctx(), sess.ID, 7, true); err != nil {
		t.Fatalf("SetArchived: %v", err)
	}
	if list, _ := s.ListSessions(ctx(), 7, false); len(list) != 0 {
		t.Errorf("an archived session is still listed by default: %+v", list)
	}
	list, err := s.ListSessions(ctx(), 7, true)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(list) != 1 || !list[0].Archived {
		t.Errorf("ListSessions(all) = %+v", list)
	}

	// Unarchiving restores it, and the Codex thread id survives both ways:
	// archiving is a listing change, not a deletion.
	if err := s.SetThreadID(ctx(), sess.ID, 7, testUUID(9)); err != nil {
		t.Fatalf("SetThreadID: %v", err)
	}
	if err := s.SetArchived(ctx(), sess.ID, 7, false); err != nil {
		t.Fatalf("SetArchived(false): %v", err)
	}
	got, _ = s.GetSession(ctx(), sess.ID, 7)
	if got.Archived || got.CodexThreadID != testUUID(9) {
		t.Errorf("after unarchiving: %+v", got)
	}
}

func TestSetThreadIDRejectsANonUUID(t *testing.T) {
	s := openStore(t)
	sess, _ := s.CreateSession(ctx(), 1, "x", "/ws")
	for _, bad := range []string{"", "my-thread-name", testUUID(1) + "x", testUUID(1)[:35], "0199a213-81c0-7800-8aa1-zzzzzzzzzzzz"} {
		if err := s.SetThreadID(ctx(), sess.ID, 1, bad); err == nil {
			t.Errorf("SetThreadID accepted %q, which is not a UUID", bad)
		}
	}
	if err := s.SetThreadID(ctx(), sess.ID, 1, testUUID(2)); err != nil {
		t.Errorf("SetThreadID rejected a valid UUID: %v", err)
	}
}

func TestSelectionIsScopedByChatTopicAndUser(t *testing.T) {
	s := openStore(t)
	const user = int64(111)
	chatA, chatB := int64(111), int64(999)

	a, _ := s.CreateSession(ctx(), user, "in chat A", "/ws")
	b, _ := s.CreateSession(ctx(), user, "in chat B", "/ws")
	topic, _ := s.CreateSession(ctx(), user, "in a topic", "/ws")

	if err := s.SelectSession(ctx(), chatA, 0, user, a.ID); err != nil {
		t.Fatalf("select in chat A: %v", err)
	}
	if err := s.SelectSession(ctx(), chatB, 0, user, b.ID); err != nil {
		t.Fatalf("select in chat B: %v", err)
	}
	if err := s.SelectSession(ctx(), chatA, 42, user, topic.ID); err != nil {
		t.Fatalf("select in a topic: %v", err)
	}

	// Each scope keeps its own choice, so one user can hold several
	// conversations with the bot at once.
	for _, c := range []struct {
		chat, thread int64
		want         string
	}{
		{chatA, 0, a.ID},
		{chatB, 0, b.ID},
		{chatA, 42, topic.ID},
		{chatA, 43, ""}, // a different topic has no selection of its own
	} {
		got, err := s.GetSelection(ctx(), c.chat, c.thread, user)
		if c.want == "" {
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("GetSelection(%d,%d) = %q, %v; want ErrNotFound", c.chat, c.thread, got, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("GetSelection(%d,%d): %v", c.chat, c.thread, err)
			continue
		}
		if got != c.want {
			t.Errorf("GetSelection(%d,%d) = %q, want %q", c.chat, c.thread, got, c.want)
		}
	}

	// Another user in the same chat id (not possible in a private chat, but the
	// row key must still separate them) sees no selection.
	if _, err := s.GetSelection(ctx(), chatA, 0, 222); !errors.Is(err, ErrNotFound) {
		t.Errorf("another user reads a selection that is not theirs: %v", err)
	}
}

func TestSelectedSessionRechecksOwnership(t *testing.T) {
	s := openStore(t)
	alice, _ := s.CreateSession(ctx(), 111, "alice", "/ws")
	if err := s.SelectSession(ctx(), 111, 0, 111, alice.ID); err != nil {
		t.Fatalf("select: %v", err)
	}
	if _, err := s.SelectedSession(ctx(), 111, 0, 222); !errors.Is(err, ErrNotFound) {
		t.Errorf("SelectedSession as another user = %v, want ErrNotFound", err)
	}
}

func TestClearSelection(t *testing.T) {
	s := openStore(t)
	sess, _ := s.CreateSession(ctx(), 1, "x", "/ws")
	if err := s.SelectSession(ctx(), 1, 0, 1, sess.ID); err != nil {
		t.Fatalf("select: %v", err)
	}
	if err := s.ClearSelection(ctx(), 1, 0, 1); err != nil {
		t.Fatalf("ClearSelection: %v", err)
	}
	if _, err := s.GetSelection(ctx(), 1, 0, 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("the selection survived ClearSelection: %v", err)
	}
}

func TestClaimUpdateIsIdempotent(t *testing.T) {
	s := openStore(t)
	first, err := s.ClaimUpdate(ctx(), 5001, 111, 111, "text")
	if err != nil {
		t.Fatalf("ClaimUpdate: %v", err)
	}
	if !first {
		t.Fatal("the first claim reported the update as already taken")
	}
	for i := 0; i < 5; i++ {
		again, err := s.ClaimUpdate(ctx(), 5001, 111, 111, "text")
		if err != nil {
			t.Fatalf("ClaimUpdate %d: %v", i, err)
		}
		if again {
			t.Fatalf("claim %d succeeded twice for one update id", i)
		}
	}
	claimed, err := s.UpdateClaimed(ctx(), 5001)
	if err != nil || !claimed {
		t.Errorf("UpdateClaimed = %v, %v", claimed, err)
	}
	if claimed, _ := s.UpdateClaimed(ctx(), 5002); claimed {
		t.Error("UpdateClaimed reports an update that was never claimed")
	}
	if _, err := s.ClaimUpdate(ctx(), 0, 1, 1, "text"); err == nil {
		t.Error("ClaimUpdate accepted update id 0")
	}
}

func TestPendingRepliesKeepTopicAndInferLegacyScope(t *testing.T) {
	s := openStore(t)
	sess, err := s.CreateSession(ctx(), 111, "reply", "/ws")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SelectSession(ctx(), 111, 19, 111, sess.ID); err != nil {
		t.Fatal(err)
	}
	for _, updateID := range []int64{1, 2} {
		if _, err := s.ClaimUpdateInThread(ctx(), updateID, 111, 111, 19, "text"); err != nil {
			t.Fatal(err)
		}
		turnID, err := s.BeginTurn(ctx(), updateID, sess.ID, 111, 4, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.FinishTurn(ctx(), turnID, TurnCompleted, "", "answer", "", 0); err != nil {
			t.Fatal(err)
		}
	}
	// An existing database has no recorded topic on its old claims.
	if _, err := s.DB().ExecContext(ctx(), `UPDATE processed_updates SET thread_id = NULL WHERE update_id = 2`); err != nil {
		t.Fatal(err)
	}
	pending, err := s.PendingReplies(ctx(), 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[0].UpdateID != 1 || pending[0].Legacy ||
		pending[1].UpdateID != 2 || !pending[1].Legacy || !pending[1].TopicKnown ||
		pending[1].ThreadID != 19 || pending[1].ChatID != 111 {
		t.Fatalf("pending replies = %+v", pending)
	}
	if err := s.ClearSelection(ctx(), 111, 19, 111); err != nil {
		t.Fatal(err)
	}
	pending, err = s.PendingReplies(ctx(), 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 || pending[1].TopicKnown || pending[1].ThreadID != 0 {
		t.Fatalf("legacy reply without selection must use the chat root: %+v", pending)
	}
}

// TestClaimSurvivesReopen is the durability half of the deduplication guarantee:
// a claimed update must still be claimed after the process exits and a new one
// opens the same database.
func TestClaimSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/bot.db"

	first, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if ok, err := first.ClaimUpdate(ctx(), 77, 1, 1, "text"); err != nil || !ok {
		t.Fatalf("ClaimUpdate = %v, %v", ok, err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()

	ok, err := second.ClaimUpdate(ctx(), 77, 1, 1, "text")
	if err != nil {
		t.Fatalf("ClaimUpdate after reopen: %v", err)
	}
	if ok {
		t.Fatal("a claimed update was claimable again after a restart: Codex would run twice")
	}
}

func TestBeginTurnRejectsASecondTurnForOneUpdate(t *testing.T) {
	s := openStore(t)
	sess, _ := s.CreateSession(ctx(), 1, "x", "/ws")

	id, err := s.BeginTurn(ctx(), 900, sess.ID, 1, 12, []string{"exec", "--json"})
	if err != nil {
		t.Fatalf("BeginTurn: %v", err)
	}
	if id <= 0 {
		t.Errorf("BeginTurn returned id %d", id)
	}
	if _, err := s.BeginTurn(ctx(), 900, sess.ID, 1, 12, []string{"exec"}); !errors.Is(err, ErrAlreadyClaimed) {
		t.Errorf("the second BeginTurn for one update = %v, want ErrAlreadyClaimed", err)
	}
	// A different update on the same session is fine.
	if _, err := s.BeginTurn(ctx(), 901, sess.ID, 1, 12, nil); err != nil {
		t.Errorf("BeginTurn for another update: %v", err)
	}
}

func TestTurnLifecycle(t *testing.T) {
	s := openStore(t)
	sess, _ := s.CreateSession(ctx(), 1, "x", "/ws")
	argv := []string{"exec", "--json", "--", "<prompt>"}

	id, err := s.BeginTurn(ctx(), 1, sess.ID, 1, 5, argv)
	if err != nil {
		t.Fatalf("BeginTurn: %v", err)
	}
	running, err := s.HasRunningTurn(ctx(), sess.ID)
	if err != nil || !running {
		t.Errorf("HasRunningTurn = %v, %v; want true", running, err)
	}

	// A non-terminal status must be refused: it would leave a row that claims a
	// process is alive when none is.
	if err := s.FinishTurn(ctx(), id, TurnRunning, "", "", "", 0); err == nil {
		t.Error("FinishTurn accepted a non-terminal status")
	}

	reply := "the answer"
	if err := s.FinishTurn(ctx(), id, TurnCompleted, testUUID(3), reply, "", 0); err != nil {
		t.Fatalf("FinishTurn: %v", err)
	}
	got, err := s.TurnByUpdate(ctx(), 1)
	if err != nil {
		t.Fatalf("TurnByUpdate: %v", err)
	}
	if got.Status != TurnCompleted || got.Reply != reply || got.CodexThreadID != testUUID(3) {
		t.Errorf("turn = %+v", got)
	}
	if len(got.Argv) != len(argv) || got.Argv[1] != "--json" {
		t.Errorf("turn argv = %q, want %q", got.Argv, argv)
	}
	if got.FinishedAt.IsZero() || got.FinishedAt.Before(got.StartedAt) {
		t.Errorf("turn timestamps = %s..%s", got.StartedAt, got.FinishedAt)
	}
	if got.Delivered {
		t.Error("a finished turn is marked delivered before anything was sent")
	}
	if err := s.MarkTurnDelivered(ctx(), id); err != nil {
		t.Fatalf("MarkTurnDelivered: %v", err)
	}
	got, _ = s.TurnByUpdate(ctx(), 1)
	if !got.Delivered {
		t.Error("MarkTurnDelivered did not stick")
	}
	if running, _ := s.HasRunningTurn(ctx(), sess.ID); running {
		t.Error("the session still reports a running turn after it finished")
	}
	if _, err := s.TurnByUpdate(ctx(), 2); !errors.Is(err, ErrNotFound) {
		t.Errorf("TurnByUpdate for an unknown update = %v, want ErrNotFound", err)
	}
	if err := s.MarkTurnDelivered(ctx(), 9999); !errors.Is(err, ErrNotFound) {
		t.Errorf("MarkTurnDelivered for an unknown turn = %v, want ErrNotFound", err)
	}
}

func TestLastTurnForSessionUsesInsertionOrder(t *testing.T) {
	s := openStore(t)
	sess, _ := s.CreateSession(ctx(), 1, "x", "/ws")
	// Three turns inside one clock tick: the fixed test clock makes the
	// timestamps identical, so only the id can order them.
	fixed := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	s.SetClock(func() time.Time { return fixed })

	for i, status := range []string{TurnCompleted, TurnFailed, TurnCancelled} {
		id, err := s.BeginTurn(ctx(), int64(100+i), sess.ID, 1, 1, nil)
		if err != nil {
			t.Fatalf("BeginTurn %d: %v", i, err)
		}
		if err := s.FinishTurn(ctx(), id, status, "", "", "", 0); err != nil {
			t.Fatalf("FinishTurn %d: %v", i, err)
		}
	}
	last, err := s.LastTurnForSession(ctx(), sess.ID)
	if err != nil {
		t.Fatalf("LastTurnForSession: %v", err)
	}
	if last.Status != TurnCancelled {
		t.Errorf("last turn status = %q, want the most recently inserted one", last.Status)
	}
}

func TestMarkInterruptedTurns(t *testing.T) {
	s := openStore(t)
	sess, _ := s.CreateSession(ctx(), 1, "x", "/ws")

	// Two turns left 'running' by a dead process, and one that finished.
	if _, err := s.BeginTurn(ctx(), 1, sess.ID, 1, 1, nil); err != nil {
		t.Fatalf("BeginTurn: %v", err)
	}
	if _, err := s.BeginTurn(ctx(), 2, sess.ID, 1, 1, nil); err != nil {
		t.Fatalf("BeginTurn: %v", err)
	}
	done, err := s.BeginTurn(ctx(), 3, sess.ID, 1, 1, nil)
	if err != nil {
		t.Fatalf("BeginTurn: %v", err)
	}
	if err := s.FinishTurn(ctx(), done, TurnCompleted, testUUID(1), "answer", "", 0); err != nil {
		t.Fatalf("FinishTurn: %v", err)
	}

	n, err := s.MarkInterruptedTurns(ctx())
	if err != nil {
		t.Fatalf("MarkInterruptedTurns: %v", err)
	}
	if n != 2 {
		t.Errorf("MarkInterruptedTurns = %d, want 2", n)
	}
	// Idempotent: a second startup finds nothing new.
	if n, err := s.MarkInterruptedTurns(ctx()); err != nil || n != 0 {
		t.Errorf("the second MarkInterruptedTurns = %d, %v; want 0", n, err)
	}
	for _, updateID := range []int64{1, 2} {
		got, err := s.TurnByUpdate(ctx(), updateID)
		if err != nil {
			t.Fatalf("TurnByUpdate(%d): %v", updateID, err)
		}
		if got.Status != TurnInterrupted {
			t.Errorf("turn %d status = %q, want interrupted", updateID, got.Status)
		}
		if !strings.Contains(got.Error, "restarted") {
			t.Errorf("turn %d has no explanation: %q", updateID, got.Error)
		}
		if got.FinishedAt.IsZero() {
			t.Errorf("turn %d has no finish time", updateID)
		}
	}
	kept, _ := s.TurnByUpdate(ctx(), 3)
	if kept.Status != TurnCompleted || kept.Reply != "answer" {
		t.Errorf("a completed turn was rewritten: %+v", kept)
	}
}

func TestOffsetOnlyMovesForward(t *testing.T) {
	s := openStore(t)
	if got, err := s.GetOffset(ctx()); err != nil || got != 0 {
		t.Fatalf("GetOffset = %d, %v; want 0", got, err)
	}
	if err := s.AdvanceOffset(ctx(), 100); err != nil {
		t.Fatalf("AdvanceOffset: %v", err)
	}
	if got, _ := s.GetOffset(ctx()); got != 100 {
		t.Errorf("GetOffset = %d, want 100", got)
	}
	// A bug that tried to rewind must not replay history and re-run old prompts.
	if err := s.AdvanceOffset(ctx(), 5); err != nil {
		t.Fatalf("AdvanceOffset backwards: %v", err)
	}
	if got, _ := s.GetOffset(ctx()); got != 100 {
		t.Errorf("the offset moved backwards to %d", got)
	}
	if err := s.AdvanceOffset(ctx(), 100); err != nil {
		t.Fatalf("AdvanceOffset to the same value: %v", err)
	}
	if err := s.AdvanceOffset(ctx(), 101); err != nil {
		t.Fatalf("AdvanceOffset: %v", err)
	}
	if got, _ := s.GetOffset(ctx()); got != 101 {
		t.Errorf("GetOffset = %d, want 101", got)
	}
}

// TestOffsetSurvivesReopen completes the guarantee: after a restart the poller
// resumes where it left off instead of asking Telegram for history again.
func TestOffsetSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/bot.db"

	first, err := Open(path, nil)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := first.AdvanceOffset(ctx(), 4242); err != nil {
		t.Fatalf("AdvanceOffset: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	second, err := Open(path, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer second.Close()
	if got, err := second.GetOffset(ctx()); err != nil || got != 4242 {
		t.Errorf("after reopening, GetOffset = %d, %v; want 4242", got, err)
	}
}

func TestMetaRoundTrip(t *testing.T) {
	s := openStore(t)
	if got, err := s.GetMeta(ctx(), "absent"); err != nil || got != "" {
		t.Errorf("GetMeta(absent) = %q, %v", got, err)
	}
	if err := s.SetMeta(ctx(), "k", "v1"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if err := s.SetMeta(ctx(), "k", "v2"); err != nil {
		t.Fatalf("SetMeta update: %v", err)
	}
	got, err := s.GetMeta(ctx(), "k")
	if err != nil || got != "v2" {
		t.Errorf("GetMeta = %q, %v; want v2", got, err)
	}
}

func TestTerminalStatuses(t *testing.T) {
	for _, status := range []string{TurnCompleted, TurnFailed, TurnCancelled, TurnTimeout, TurnBusy, TurnInterrupted} {
		if !Terminal(status) {
			t.Errorf("Terminal(%q) = false", status)
		}
	}
	for _, status := range []string{TurnRunning, "", "nonsense"} {
		if Terminal(status) {
			t.Errorf("Terminal(%q) = true", status)
		}
	}
}

func TestOpenRejectsAnEmptyPath(t *testing.T) {
	if _, err := Open("", nil); err == nil {
		t.Error("Open accepted an empty path")
	}
}

func testUUID(seed int) string {
	const hex = "0123456789abcdef"
	last := make([]byte, 12)
	for i := range last {
		last[i] = hex[(seed+i)%len(hex)]
	}
	return "0199a213-81c0-7800-8aa1-" + string(last)
}

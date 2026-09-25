// Package store is the bot's persistence layer: SQLite behind narrow methods,
// with every query that touches a session carrying its owner.
//
// Three properties matter more than anything else here:
//
//   - Durability of the deduplication record. A Telegram update is claimed in
//     processed_updates and committed before Codex is started, and the poller
//     only advances its offset afterwards. That ordering is what makes "a
//     network retry or a restart must not turn one Telegram message into a
//     second Codex request" true.
//   - Ownership in the query, not in the caller. GetSession takes the owner id
//     as a parameter and filters on it, so a session id is never a capability:
//     guessing "s7k3qm" gets a user somebody else's ErrNotFound.
//   - Restart visibility. Turns are rows with a status, so a process that died
//     mid-turn leaves a 'running' row that startup flips to 'interrupted' and
//     /session reports. Nothing is silently forgotten.
//
// Timestamps are stored as RFC 3339 UTC text and compared in Go, never in SQL,
// so textual ordering never decides anything.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, registered as "sqlite"

	"codex-telegram-bot/migrations"
)

// DriverName is the database/sql driver this build uses. It is pure Go, so the
// binary needs no libsqlite3 and no cgo toolchain on the host.
const DriverName = "sqlite"

// ErrNotFound means the requested row does not exist — or exists and belongs to
// somebody else, which is deliberately indistinguishable from the outside.
var ErrNotFound = errors.New("not found")

// ErrAlreadyClaimed means an update id was already taken by this bot.
var ErrAlreadyClaimed = errors.New("update already claimed")

// Store owns the database handle.
type Store struct {
	db  *sql.DB
	log *slog.Logger
	now func() time.Time
}

// Open opens (creating if needed) the database at path and applies migrations.
//
// The file is created 0600 and its directory is expected to be 0700 already;
// internal/config guarantees that for BOT_STATE_DIR. The database contains
// conversation replies, so it is treated like the credential cache it sits next
// to.
func Open(path string, log *slog.Logger) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: database path is required")
	}
	if log == nil {
		log = slog.Default()
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := mkdirPrivate(dir); err != nil {
			return nil, fmt.Errorf("store: prepare %s: %w", dir, err)
		}
	}

	// Create the file 0600 before SQLite does. It would otherwise create it
	// 0644 (masked by the process umask), and this database holds conversation
	// replies: on a shared host that is other people's business.
	if err := createPrivate(path); err != nil {
		return nil, fmt.Errorf("store: create %s: %w", path, err)
	}

	db, err := sql.Open(DriverName, dsn(path))
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// One connection: every write is serialised through a single handle, which
	// removes SQLITE_BUSY as a failure mode entirely. The bot's write volume is
	// a handful of rows per Telegram message, so this costs nothing.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	s := &Store{db: db, log: log, now: func() time.Time { return time.Now().UTC() }}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	// WAL mode adds -wal and -shm files, created by SQLite rather than by us, so
	// they need the same treatment once they exist.
	s.tightenPermissions(path)
	return s, nil
}

// tightenPermissions makes the database and its WAL sidecars private.
//
// Failures are logged rather than returned: a filesystem that refuses chmod
// should not stop the bot, and logStartup warns about an open state directory
// anyway.
func (s *Store) tightenPermissions(path string) {
	for _, p := range []string{path, path + "-wal", path + "-shm", path + "-journal"} {
		if _, err := os.Stat(p); err != nil {
			continue // not created (yet)
		}
		if err := os.Chmod(p, 0o600); err != nil {
			s.log.Warn("could not restrict the permissions of a database file",
				"path", p, "error", err.Error())
		}
	}
}

// dsn builds the SQLite connection string.
//
// synchronous=FULL is not the default and is chosen on purpose: the guarantee
// the bot needs is that a committed processed_updates row survives a power cut,
// not just a process crash. At this write volume the cost is invisible.
func dsn(path string) string {
	q := url.Values{}
	q.Set("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "synchronous(FULL)")
	u := url.URL{Scheme: "file", Opaque: path, RawQuery: q.Encode()}
	return u.String()
}

func mkdirPrivate(dir string) error {
	if err := mkdirAll(dir, 0o700); err != nil {
		return err
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// DB exposes the handle for tests that want to inspect the schema.
func (s *Store) DB() *sql.DB { return s.db }

// Now returns the store's current time. Tests replace it.
func (s *Store) Now() time.Time { return s.now() }

// SetClock replaces the time source; tests use it, production does not.
func (s *Store) SetClock(f func() time.Time) {
	if f != nil {
		s.now = f
	}
}

func (s *Store) timestamp() string { return rfc3339(s.now()) }

// rfc3339 formats t as RFC 3339 in UTC, which is the only timestamp format
// stored.
func rfc3339(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// parseTime reads a stored timestamp, tolerating an empty value.
func parseTime(s string) (time.Time, error) {
	if strings.TrimSpace(s) == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}

// --- migrations ------------------------------------------------------------

// migrate applies every embedded migration that has not run yet, in one
// transaction. Either the whole set lands or none of it does, which means a
// half-migrated database cannot exist.
func (s *Store) migrate(ctx context.Context) error {
	const bootstrap = `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL
	)`

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin migration: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	if _, err := tx.ExecContext(ctx, bootstrap); err != nil {
		return fmt.Errorf("store: create schema_migrations: %w", err)
	}

	applied := map[string]bool{}
	rows, err := tx.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("store: read schema_migrations: %w", err)
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return fmt.Errorf("store: scan schema_migrations: %w", err)
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: iterate schema_migrations: %w", err)
	}

	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("store: read embedded migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	now := s.timestamp()
	for _, name := range names {
		if applied[name] {
			continue
		}
		sqlBytes, err := migrations.FS.ReadFile(name)
		if err != nil {
			return fmt.Errorf("store: read migration %s: %w", name, err)
		}
		s.log.Info("applying migration", "migration", name)
		if _, err := tx.ExecContext(ctx, string(sqlBytes)); err != nil {
			return fmt.Errorf("store: apply migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, name, now); err != nil {
			return fmt.Errorf("store: record migration %s: %w", name, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit migrations: %w", err)
	}
	return nil
}

// Migrations lists the embedded migration file names, in apply order. Exposed
// for the startup log line and for tests.
func Migrations() []string {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	return names
}

// --- meta key/value --------------------------------------------------------

// SetMeta stores a key.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO meta (key, value, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT (key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`,
		key, value, s.timestamp())
	if err != nil {
		return fmt.Errorf("store: set meta %q: %w", key, err)
	}
	return nil
}

// GetMeta reads a key, returning "" when it is absent.
func (s *Store) GetMeta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: get meta %q: %w", key, err)
	}
	return v, nil
}

// CountAll returns the number of sessions stored for every user. It exists for
// the startup log line, which is the only place the bot has a reason to look
// across owners.
func (s *Store) CountAll(ctx context.Context) (int, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count all sessions: %w", err)
	}
	return n, nil
}

// Ping checks the database is usable.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

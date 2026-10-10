// Package store persists flagpost's state in SQLite. It owns the schema
// (applied through versioned, checksummed migrations) and exposes typed
// methods for each entity. Writes that must be atomic, such as a config
// change together with its audit entry and the environment revision bump,
// run inside Store.Tx.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Sentinel errors returned by store methods.
var (
	ErrNotFound      = errors.New("not found")
	ErrAlreadyExists = errors.New("already exists")
	// ErrConflict means an optimistic concurrency check failed: the
	// row's version is not the one the caller based its change on.
	ErrConflict = errors.New("version conflict")
)

// querier is the subset of *sql.DB and *sql.Tx the queries need.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// Queries implements every read and write. It is embedded by Store, which
// runs each statement on its own, and by Tx, which runs them in a
// transaction.
type Queries struct {
	q   querier
	now func() time.Time
}

// Store is a handle to the database.
type Store struct {
	Queries
	db *sql.DB
}

// Tx is a transaction scoped view of the store.
type Tx struct {
	Queries
}

// Options configures Open.
type Options struct {
	// Now overrides the clock, for tests.
	Now func() time.Time
	// SkipMigrations opens the database without applying pending
	// migrations.
	SkipMigrations bool
}

// Open opens (creating if necessary) the SQLite database at path and
// applies pending migrations. The connection enables foreign keys, WAL
// journaling, a busy timeout and IMMEDIATE transactions, the last of which
// prevents the SQLITE_BUSY deadlocks that deferred transactions hit when
// two writers upgrade from a read lock at the same time.
func Open(ctx context.Context, path string, opts Options) (*Store, error) {
	if path == "" {
		return nil, errors.New("store: database path is required")
	}
	q := url.Values{}
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Set("_txlock", "immediate")
	dsn := "file:" + path + "?" + q.Encode()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(8)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: ping %s: %w", path, err)
	}
	if !opts.SkipMigrations {
		ms, err := builtinMigrations()
		if err != nil {
			db.Close()
			return nil, err
		}
		if _, err := Migrate(ctx, db, ms); err != nil {
			db.Close()
			return nil, fmt.Errorf("store: migrate: %w", err)
		}
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Store{Queries: Queries{q: db, now: now}, db: db}, nil
}

// DB exposes the underlying database handle.
func (s *Store) DB() *sql.DB { return s.db }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Ping checks that the database is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Tx runs fn in a transaction, committing if fn returns nil and rolling
// back otherwise.
func (s *Store) Tx(ctx context.Context, fn func(tx *Tx) error) error {
	sqlTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin: %w", err)
	}
	tx := &Tx{Queries: Queries{q: sqlTx, now: s.now}}
	if err := fn(tx); err != nil {
		_ = sqlTx.Rollback()
		return err
	}
	if err := sqlTx.Commit(); err != nil {
		return fmt.Errorf("store: commit: %w", err)
	}
	return nil
}

// Migrations lists the built-in migrations, for the CLI.
func Migrations() ([]Migration, error) { return builtinMigrations() }

const timeLayout = "2006-01-02T15:04:05.000000Z"

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(s string) time.Time {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		t, _ = time.Parse(time.RFC3339Nano, s)
	}
	return t
}

// timestamp returns the current time truncated to the stored precision, so
// values returned from writes equal values later read back.
func (q *Queries) timestamp() time.Time { return q.now().UTC().Truncate(time.Microsecond) }

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// likeEscape escapes LIKE wildcards in user input; queries use ESCAPE '\'.
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

func notFound(what, key string) error { return fmt.Errorf("%s %q: %w", what, key, ErrNotFound) }

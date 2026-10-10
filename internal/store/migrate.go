package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Migration is one versioned schema change.
type Migration struct {
	Version  int
	Name     string
	SQL      string
	Checksum string
}

// AppliedMigration is a row of the schema_migrations table.
type AppliedMigration struct {
	Version   int
	Name      string
	Checksum  string
	AppliedAt time.Time
}

// LoadMigrations parses migration files named NNNN_description.sql from
// fsys, sorted by version. Versions must be unique and positive.
func LoadMigrations(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}
	var out []Migration
	seen := map[int]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".sql")
		num, name, ok := strings.Cut(base, "_")
		if !ok || name == "" {
			return nil, fmt.Errorf("migration %q: want NNNN_description.sql", e.Name())
		}
		v, err := strconv.Atoi(num)
		if err != nil || v <= 0 {
			return nil, fmt.Errorf("migration %q: version must be a positive integer", e.Name())
		}
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("migrations %q and %q share version %d", prev, e.Name(), v)
		}
		seen[v] = e.Name()
		body, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(body)
		out = append(out, Migration{Version: v, Name: name, SQL: string(body), Checksum: hex.EncodeToString(sum[:])})
	}
	slices.SortFunc(out, func(a, b Migration) int { return a.Version - b.Version })
	return out, nil
}

func builtinMigrations() ([]Migration, error) {
	sub, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		return nil, err
	}
	return LoadMigrations(sub)
}

const createMigrationsTable = `CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    name       TEXT NOT NULL,
    checksum   TEXT NOT NULL,
    applied_at TEXT NOT NULL
)`

// AppliedMigrations returns the migrations recorded in the database.
func AppliedMigrations(ctx context.Context, db *sql.DB) ([]AppliedMigration, error) {
	if _, err := db.ExecContext(ctx, createMigrationsTable); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}
	rows, err := db.QueryContext(ctx, `SELECT version, name, checksum, applied_at FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AppliedMigration
	for rows.Next() {
		var m AppliedMigration
		var at string
		if err := rows.Scan(&m.Version, &m.Name, &m.Checksum, &at); err != nil {
			return nil, err
		}
		m.AppliedAt = parseTime(at)
		out = append(out, m)
	}
	return out, rows.Err()
}

// Migrate applies every pending migration in order, each in its own
// transaction. It refuses to run when an applied migration's checksum no
// longer matches its file (someone edited history) or when the database
// has migrations this binary does not know about (a newer binary has
// already upgraded it). It returns the versions it applied.
func Migrate(ctx context.Context, db *sql.DB, migrations []Migration) ([]int, error) {
	applied, err := AppliedMigrations(ctx, db)
	if err != nil {
		return nil, err
	}
	known := map[int]Migration{}
	for _, m := range migrations {
		known[m.Version] = m
	}
	done := map[int]bool{}
	for _, a := range applied {
		m, ok := known[a.Version]
		if !ok {
			return nil, fmt.Errorf("database has migration %d (%s) which this binary does not know; refusing to run an older binary against a newer schema", a.Version, a.Name)
		}
		if m.Checksum != a.Checksum {
			return nil, fmt.Errorf("migration %d (%s) was modified after being applied: checksum %s, recorded %s", a.Version, a.Name, m.Checksum[:12], a.Checksum[:min(12, len(a.Checksum))])
		}
		done[a.Version] = true
	}

	var ran []int
	for _, m := range migrations {
		if done[m.Version] {
			continue
		}
		if err := applyMigration(ctx, db, m); err != nil {
			return ran, err
		}
		ran = append(ran, m.Version)
	}
	return ran, nil
}

func applyMigration(ctx context.Context, db *sql.DB, m Migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return fmt.Errorf("apply migration %d (%s): %w", m.Version, m.Name, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`,
		m.Version, m.Name, m.Checksum, formatTime(time.Now())); err != nil {
		return fmt.Errorf("record migration %d: %w", m.Version, err)
	}
	return tx.Commit()
}

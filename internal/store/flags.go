package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/J-X0-cloud/flagpost/model"
)

const flagColumns = `key, name, description, kind, variants, tags, archived, version, created_at, updated_at`

func scanFlag(row interface{ Scan(...any) error }) (model.Flag, error) {
	var f model.Flag
	var variants, tags, created, updated string
	var archived int
	if err := row.Scan(&f.Key, &f.Name, &f.Description, &f.Kind, &variants, &tags, &archived, &f.Version, &created, &updated); err != nil {
		return f, err
	}
	if err := json.Unmarshal([]byte(variants), &f.Variants); err != nil {
		return f, fmt.Errorf("decode variants of %q: %w", f.Key, err)
	}
	if err := json.Unmarshal([]byte(tags), &f.Tags); err != nil {
		return f, fmt.Errorf("decode tags of %q: %w", f.Key, err)
	}
	f.Archived = archived != 0
	f.CreatedAt, f.UpdatedAt = parseTime(created), parseTime(updated)
	return f, nil
}

func encodeFlagJSON(f *model.Flag) (variants, tags string, err error) {
	v, err := json.Marshal(f.Variants)
	if err != nil {
		return "", "", err
	}
	t := f.Tags
	if t == nil {
		t = []string{}
	}
	tb, err := json.Marshal(t)
	if err != nil {
		return "", "", err
	}
	return string(v), string(tb), nil
}

// CreateFlag inserts a new flag at version 1.
func (q *Queries) CreateFlag(ctx context.Context, f model.Flag) (model.Flag, error) {
	if _, err := q.GetFlag(ctx, f.Key); err == nil {
		return f, fmt.Errorf("flag %q: %w", f.Key, ErrAlreadyExists)
	} else if !errors.Is(err, ErrNotFound) {
		return f, err
	}
	variants, tags, err := encodeFlagJSON(&f)
	if err != nil {
		return f, err
	}
	now := q.timestamp()
	f.Version, f.CreatedAt, f.UpdatedAt = 1, now, now
	_, err = q.q.ExecContext(ctx,
		`INSERT INTO flags (`+flagColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		f.Key, f.Name, f.Description, f.Kind, variants, tags, boolInt(f.Archived), f.Version,
		formatTime(now), formatTime(now))
	if err != nil {
		return f, fmt.Errorf("insert flag: %w", err)
	}
	return f, nil
}

// GetFlag returns the flag with the given key.
func (q *Queries) GetFlag(ctx context.Context, key string) (model.Flag, error) {
	f, err := scanFlag(q.q.QueryRowContext(ctx, `SELECT `+flagColumns+` FROM flags WHERE key = ?`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return f, notFound("flag", key)
	}
	return f, err
}

// FlagFilter narrows ListFlags.
type FlagFilter struct {
	// Query matches a substring of the key or name, case-insensitively.
	Query string
	// Tag restricts results to flags carrying the tag.
	Tag string
	// IncludeArchived includes archived flags.
	IncludeArchived bool
}

// ListFlags returns flags ordered by key.
func (q *Queries) ListFlags(ctx context.Context, filter FlagFilter) ([]model.Flag, error) {
	var where []string
	var args []any
	if !filter.IncludeArchived {
		where = append(where, "archived = 0")
	}
	if filter.Query != "" {
		pat := "%" + likeEscape(strings.ToLower(filter.Query)) + "%"
		where = append(where, `(lower(key) LIKE ? ESCAPE '\' OR lower(name) LIKE ? ESCAPE '\')`)
		args = append(args, pat, pat)
	}
	if filter.Tag != "" {
		where = append(where, `EXISTS (SELECT 1 FROM json_each(flags.tags) WHERE json_each.value = ?)`)
		args = append(args, filter.Tag)
	}
	query := `SELECT ` + flagColumns + ` FROM flags`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY key"
	rows, err := q.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Flag
	for rows.Next() {
		f, err := scanFlag(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// UpdateFlag replaces the mutable fields of a flag, provided its stored
// version equals expectedVersion. The returned flag carries the new
// version.
func (q *Queries) UpdateFlag(ctx context.Context, f model.Flag, expectedVersion int64) (model.Flag, error) {
	variants, tags, err := encodeFlagJSON(&f)
	if err != nil {
		return f, err
	}
	now := q.timestamp()
	res, err := q.q.ExecContext(ctx,
		`UPDATE flags SET name = ?, description = ?, kind = ?, variants = ?, tags = ?, archived = ?,
		        version = version + 1, updated_at = ?
		  WHERE key = ? AND version = ?`,
		f.Name, f.Description, f.Kind, variants, tags, boolInt(f.Archived), formatTime(now), f.Key, expectedVersion)
	if err != nil {
		return f, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := q.GetFlag(ctx, f.Key); err != nil {
			return f, err
		}
		return f, fmt.Errorf("flag %q: %w", f.Key, ErrConflict)
	}
	return q.GetFlag(ctx, f.Key)
}

// DeleteFlag removes a flag and its configuration in every environment.
func (q *Queries) DeleteFlag(ctx context.Context, key string) error {
	res, err := q.q.ExecContext(ctx, `DELETE FROM flags WHERE key = ?`, key)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return notFound("flag", key)
	}
	return nil
}

// FlagsDependingOn returns the keys of flags whose configuration in any
// environment lists key as a prerequisite, so deletion can be refused.
func (q *Queries) FlagsDependingOn(ctx context.Context, key string) ([]string, error) {
	rows, err := q.q.QueryContext(ctx, `
		SELECT DISTINCT fc.flag_key
		  FROM flag_configs fc, json_each(fc.body, '$.prerequisites') p
		 WHERE json_extract(p.value, '$.flag') = ?
		 ORDER BY fc.flag_key`, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

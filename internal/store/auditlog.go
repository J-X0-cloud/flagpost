package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/J-X0-cloud/flagpost/internal/audit"
)

// MaxAuditPage caps the number of audit entries returned per query.
const MaxAuditPage = 500

func nullableJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return string(raw)
}

// InsertAudit appends an entry to the audit log and returns it with its ID
// and timestamp filled in.
func (q *Queries) InsertAudit(ctx context.Context, e audit.Entry) (audit.Entry, error) {
	if e.At.IsZero() {
		e.At = q.timestamp()
	}
	changes := e.Changes
	if changes == nil {
		changes = []audit.Change{}
	}
	cb, err := json.Marshal(changes)
	if err != nil {
		return e, err
	}
	err = q.q.QueryRowContext(ctx, `
		INSERT INTO audit_log (at, actor, action, resource_type, resource_key, env_key, comment, before_doc, after_doc, changes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) RETURNING id`,
		formatTime(e.At), e.Actor, e.Action, e.ResourceType, e.ResourceKey, e.Environment, e.Comment,
		nullableJSON(e.Before), nullableJSON(e.After), string(cb)).Scan(&e.ID)
	if err != nil {
		return e, fmt.Errorf("insert audit entry: %w", err)
	}
	return e, nil
}

// ListAudit returns audit entries newest first.
func (q *Queries) ListAudit(ctx context.Context, f audit.Filter) ([]audit.Entry, error) {
	var where []string
	var args []any
	add := func(cond string, v any) {
		where = append(where, cond)
		args = append(args, v)
	}
	if f.Environment != "" {
		add("env_key = ?", f.Environment)
	}
	if f.ResourceType != "" {
		add("resource_type = ?", f.ResourceType)
	}
	if f.ResourceKey != "" {
		add("resource_key = ?", f.ResourceKey)
	}
	if f.Actor != "" {
		add("actor = ?", f.Actor)
	}
	if f.BeforeID > 0 {
		add("id < ?", f.BeforeID)
	}
	limit := f.Limit
	if limit <= 0 || limit > MaxAuditPage {
		limit = MaxAuditPage
	}
	query := `SELECT id, at, actor, action, resource_type, resource_key, env_key, comment, before_doc, after_doc, changes FROM audit_log`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := q.q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []audit.Entry
	for rows.Next() {
		var e audit.Entry
		var at, changes string
		var before, after sql.NullString
		if err := rows.Scan(&e.ID, &at, &e.Actor, &e.Action, &e.ResourceType, &e.ResourceKey,
			&e.Environment, &e.Comment, &before, &after, &changes); err != nil {
			return nil, err
		}
		e.At = parseTime(at)
		if before.Valid {
			e.Before = json.RawMessage(before.String)
		}
		if after.Valid {
			e.After = json.RawMessage(after.String)
		}
		if err := json.Unmarshal([]byte(changes), &e.Changes); err != nil {
			return nil, fmt.Errorf("decode audit changes %d: %w", e.ID, err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/J-X0-cloud/flagpost/model"
)

// segmentBody is the JSON document stored for a segment's membership.
type segmentBody struct {
	Included []string            `json:"included,omitempty"`
	Excluded []string            `json:"excluded,omitempty"`
	Rules    []model.SegmentRule `json:"rules,omitempty"`
}

const segmentColumns = `env_key, key, name, description, body, version, created_at, updated_at`

func scanSegment(row interface{ Scan(...any) error }) (model.Segment, error) {
	var s model.Segment
	var body, created, updated string
	if err := row.Scan(&s.Environment, &s.Key, &s.Name, &s.Description, &body, &s.Version, &created, &updated); err != nil {
		return s, err
	}
	var b segmentBody
	if err := json.Unmarshal([]byte(body), &b); err != nil {
		return s, fmt.Errorf("decode segment %s/%s: %w", s.Environment, s.Key, err)
	}
	s.Included, s.Excluded, s.Rules = b.Included, b.Excluded, b.Rules
	s.CreatedAt, s.UpdatedAt = parseTime(created), parseTime(updated)
	return s, nil
}

func encodeSegmentBody(s *model.Segment) (string, error) {
	b, err := json.Marshal(segmentBody{Included: s.Included, Excluded: s.Excluded, Rules: s.Rules})
	return string(b), err
}

// CreateSegment inserts a segment at version 1.
func (q *Queries) CreateSegment(ctx context.Context, s model.Segment) (model.Segment, error) {
	if _, err := q.GetSegment(ctx, s.Environment, s.Key); err == nil {
		return s, fmt.Errorf("segment %q: %w", s.Key, ErrAlreadyExists)
	} else if !errors.Is(err, ErrNotFound) {
		return s, err
	}
	body, err := encodeSegmentBody(&s)
	if err != nil {
		return s, err
	}
	now := q.timestamp()
	_, err = q.q.ExecContext(ctx,
		`INSERT INTO segments (`+segmentColumns+`) VALUES (?, ?, ?, ?, ?, 1, ?, ?)`,
		s.Environment, s.Key, s.Name, s.Description, body, formatTime(now), formatTime(now))
	if err != nil {
		return s, fmt.Errorf("insert segment: %w", err)
	}
	return q.GetSegment(ctx, s.Environment, s.Key)
}

// GetSegment returns one segment.
func (q *Queries) GetSegment(ctx context.Context, env, key string) (model.Segment, error) {
	s, err := scanSegment(q.q.QueryRowContext(ctx,
		`SELECT `+segmentColumns+` FROM segments WHERE env_key = ? AND key = ?`, env, key))
	if errors.Is(err, sql.ErrNoRows) {
		return s, notFound("segment", env+"/"+key)
	}
	return s, err
}

// ListSegments returns an environment's segments ordered by key.
func (q *Queries) ListSegments(ctx context.Context, env string) ([]model.Segment, error) {
	rows, err := q.q.QueryContext(ctx, `SELECT `+segmentColumns+` FROM segments WHERE env_key = ? ORDER BY key`, env)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Segment
	for rows.Next() {
		s, err := scanSegment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// UpdateSegment replaces a segment if its stored version equals
// expectedVersion.
func (q *Queries) UpdateSegment(ctx context.Context, s model.Segment, expectedVersion int64) (model.Segment, error) {
	body, err := encodeSegmentBody(&s)
	if err != nil {
		return s, err
	}
	res, err := q.q.ExecContext(ctx,
		`UPDATE segments SET name = ?, description = ?, body = ?, version = version + 1, updated_at = ?
		  WHERE env_key = ? AND key = ? AND version = ?`,
		s.Name, s.Description, body, formatTime(q.timestamp()), s.Environment, s.Key, expectedVersion)
	if err != nil {
		return s, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if _, err := q.GetSegment(ctx, s.Environment, s.Key); err != nil {
			return s, err
		}
		return s, fmt.Errorf("segment %q: %w", s.Key, ErrConflict)
	}
	return q.GetSegment(ctx, s.Environment, s.Key)
}

// DeleteSegment removes a segment.
func (q *Queries) DeleteSegment(ctx context.Context, env, key string) error {
	res, err := q.q.ExecContext(ctx, `DELETE FROM segments WHERE env_key = ? AND key = ?`, env, key)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return notFound("segment", env+"/"+key)
	}
	return nil
}

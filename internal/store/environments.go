package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/J-X0-cloud/flagpost/model"
)

const envColumns = `key, name, description, sdk_key, revision, created_at`

func scanEnvironment(row interface{ Scan(...any) error }) (model.Environment, error) {
	var e model.Environment
	var created string
	if err := row.Scan(&e.Key, &e.Name, &e.Description, &e.SDKKey, &e.Revision, &created); err != nil {
		return e, err
	}
	e.CreatedAt = parseTime(created)
	return e, nil
}

// CreateEnvironment inserts e. SDKKey must already be set.
func (q *Queries) CreateEnvironment(ctx context.Context, e model.Environment) (model.Environment, error) {
	if _, err := q.GetEnvironment(ctx, e.Key); err == nil {
		return e, fmt.Errorf("environment %q: %w", e.Key, ErrAlreadyExists)
	} else if !errors.Is(err, ErrNotFound) {
		return e, err
	}
	e.CreatedAt = q.timestamp()
	e.Revision = 1
	_, err := q.q.ExecContext(ctx,
		`INSERT INTO environments (`+envColumns+`) VALUES (?, ?, ?, ?, ?, ?)`,
		e.Key, e.Name, e.Description, e.SDKKey, e.Revision, formatTime(e.CreatedAt))
	if err != nil {
		return e, fmt.Errorf("insert environment: %w", err)
	}
	return e, nil
}

// GetEnvironment returns the environment with the given key.
func (q *Queries) GetEnvironment(ctx context.Context, key string) (model.Environment, error) {
	e, err := scanEnvironment(q.q.QueryRowContext(ctx, `SELECT `+envColumns+` FROM environments WHERE key = ?`, key))
	if errors.Is(err, sql.ErrNoRows) {
		return e, notFound("environment", key)
	}
	return e, err
}

// GetEnvironmentBySDKKey resolves an SDK key to its environment.
func (q *Queries) GetEnvironmentBySDKKey(ctx context.Context, sdkKey string) (model.Environment, error) {
	e, err := scanEnvironment(q.q.QueryRowContext(ctx, `SELECT `+envColumns+` FROM environments WHERE sdk_key = ?`, sdkKey))
	if errors.Is(err, sql.ErrNoRows) {
		return e, fmt.Errorf("sdk key: %w", ErrNotFound)
	}
	return e, err
}

// ListEnvironments returns all environments ordered by creation time.
func (q *Queries) ListEnvironments(ctx context.Context) ([]model.Environment, error) {
	rows, err := q.q.QueryContext(ctx, `SELECT `+envColumns+` FROM environments ORDER BY created_at, key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Environment
	for rows.Next() {
		e, err := scanEnvironment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// UpdateEnvironment changes the name and description of an environment.
func (q *Queries) UpdateEnvironment(ctx context.Context, key, name, description string) (model.Environment, error) {
	res, err := q.q.ExecContext(ctx, `UPDATE environments SET name = ?, description = ? WHERE key = ?`, name, description, key)
	if err != nil {
		return model.Environment{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return model.Environment{}, notFound("environment", key)
	}
	return q.GetEnvironment(ctx, key)
}

// SetSDKKey replaces an environment's SDK key.
func (q *Queries) SetSDKKey(ctx context.Context, key, sdkKey string) error {
	res, err := q.q.ExecContext(ctx, `UPDATE environments SET sdk_key = ? WHERE key = ?`, sdkKey, key)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return notFound("environment", key)
	}
	return nil
}

// DeleteEnvironment removes an environment and, through cascading foreign
// keys, all of its flag configuration, segments and experiment events.
func (q *Queries) DeleteEnvironment(ctx context.Context, key string) error {
	res, err := q.q.ExecContext(ctx, `DELETE FROM environments WHERE key = ?`, key)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return notFound("environment", key)
	}
	return nil
}

// BumpRevision increments an environment's revision and returns the new
// value. Every change visible to SDKs bumps the revision, which doubles as
// the snapshot ETag.
func (q *Queries) BumpRevision(ctx context.Context, key string) (int64, error) {
	var rev int64
	err := q.q.QueryRowContext(ctx,
		`UPDATE environments SET revision = revision + 1 WHERE key = ? RETURNING revision`, key).Scan(&rev)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, notFound("environment", key)
	}
	return rev, err
}

// BumpAllRevisions increments every environment's revision, for changes to
// flag definitions, which are shared by all environments. It returns the
// new revision of each environment.
func (q *Queries) BumpAllRevisions(ctx context.Context) (map[string]int64, error) {
	rows, err := q.q.QueryContext(ctx, `UPDATE environments SET revision = revision + 1 RETURNING key, revision`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var k string
		var r int64
		if err := rows.Scan(&k, &r); err != nil {
			return nil, err
		}
		out[k] = r
	}
	return out, rows.Err()
}

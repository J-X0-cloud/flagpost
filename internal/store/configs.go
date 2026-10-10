package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/J-X0-cloud/flagpost/model"
)

func scanConfig(row interface{ Scan(...any) error }) (model.FlagConfig, error) {
	var c model.FlagConfig
	var flagKey, envKey, body, updated string
	var enabled int
	var version int64
	if err := row.Scan(&flagKey, &envKey, &enabled, &body, &version, &updated); err != nil {
		return c, err
	}
	if err := json.Unmarshal([]byte(body), &c); err != nil {
		return c, fmt.Errorf("decode config %s/%s: %w", envKey, flagKey, err)
	}
	// Columns are authoritative over anything duplicated in the body.
	c.FlagKey, c.Environment = flagKey, envKey
	c.Enabled = enabled != 0
	c.Version = version
	c.UpdatedAt = parseTime(updated)
	return c, nil
}

const configColumns = `flag_key, env_key, enabled, body, version, updated_at`

// GetFlagConfig returns a flag's configuration in an environment. It
// returns ErrNotFound when the flag has never been configured there.
func (q *Queries) GetFlagConfig(ctx context.Context, flagKey, env string) (model.FlagConfig, error) {
	c, err := scanConfig(q.q.QueryRowContext(ctx,
		`SELECT `+configColumns+` FROM flag_configs WHERE flag_key = ? AND env_key = ?`, flagKey, env))
	if errors.Is(err, sql.ErrNoRows) {
		return c, notFound("flag config", env+"/"+flagKey)
	}
	return c, err
}

// ListFlagConfigs returns every stored configuration in an environment,
// keyed by flag.
func (q *Queries) ListFlagConfigs(ctx context.Context, env string) (map[string]model.FlagConfig, error) {
	rows, err := q.q.QueryContext(ctx, `SELECT `+configColumns+` FROM flag_configs WHERE env_key = ?`, env)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]model.FlagConfig{}
	for rows.Next() {
		c, err := scanConfig(rows)
		if err != nil {
			return nil, err
		}
		out[c.FlagKey] = c
	}
	return out, rows.Err()
}

// PutFlagConfig stores a configuration with optimistic concurrency. An
// expectedVersion of 0 means the caller believes no configuration exists
// yet; otherwise the stored version must equal expectedVersion. The
// returned configuration carries its new version.
func (q *Queries) PutFlagConfig(ctx context.Context, c model.FlagConfig, expectedVersion int64) (model.FlagConfig, error) {
	now := q.timestamp()
	body, err := json.Marshal(c)
	if err != nil {
		return c, err
	}
	var res sql.Result
	if expectedVersion == 0 {
		res, err = q.q.ExecContext(ctx,
			`INSERT INTO flag_configs (`+configColumns+`) VALUES (?, ?, ?, ?, 1, ?)
			 ON CONFLICT (flag_key, env_key) DO NOTHING`,
			c.FlagKey, c.Environment, boolInt(c.Enabled), string(body), formatTime(now))
	} else {
		res, err = q.q.ExecContext(ctx,
			`UPDATE flag_configs SET enabled = ?, body = ?, version = version + 1, updated_at = ?
			  WHERE flag_key = ? AND env_key = ? AND version = ?`,
			boolInt(c.Enabled), string(body), formatTime(now), c.FlagKey, c.Environment, expectedVersion)
	}
	if err != nil {
		return c, fmt.Errorf("store flag config: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		if expectedVersion != 0 {
			if _, err := q.GetFlagConfig(ctx, c.FlagKey, c.Environment); err != nil {
				return c, err
			}
		}
		return c, fmt.Errorf("flag config %s/%s: %w", c.Environment, c.FlagKey, ErrConflict)
	}
	return q.GetFlagConfig(ctx, c.FlagKey, c.Environment)
}

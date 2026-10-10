package store

import (
	"context"
	"fmt"
	"time"
)

// Exposure records that a context was served a variant of an experiment.
type Exposure struct {
	Flag       string
	ContextKey string
	Variant    string
	At         time.Time
}

// Conversion records that a context performed a tracked event.
type Conversion struct {
	Event      string
	ContextKey string
	At         time.Time
}

// VariantCount is the per-variant aggregate used for experiment analysis.
type VariantCount struct {
	Variant   string
	Exposed   int64
	Converted int64
}

// RecordExposures stores exposures, keeping for each context the earliest
// one. Batches can arrive out of order from several SDK instances, so a
// later-arriving but earlier-timestamped exposure replaces the stored one
// rather than being dropped.
func (q *Queries) RecordExposures(ctx context.Context, env string, xs []Exposure) error {
	for _, x := range xs {
		_, err := q.q.ExecContext(ctx, `
			INSERT INTO exposures (env_key, flag_key, context_key, variant, first_seen)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (env_key, flag_key, context_key) DO UPDATE SET
			    variant    = CASE WHEN excluded.first_seen < exposures.first_seen THEN excluded.variant ELSE exposures.variant END,
			    first_seen = min(exposures.first_seen, excluded.first_seen)`,
			env, x.Flag, x.ContextKey, x.Variant, x.At.UnixMicro())
		if err != nil {
			return fmt.Errorf("record exposure: %w", err)
		}
	}
	return nil
}

// RecordConversions stores conversions, keeping the earliest per context
// and event.
func (q *Queries) RecordConversions(ctx context.Context, env string, cs []Conversion) error {
	for _, c := range cs {
		_, err := q.q.ExecContext(ctx, `
			INSERT INTO conversions (env_key, event_key, context_key, first_seen)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (env_key, event_key, context_key) DO UPDATE SET
			    first_seen = min(conversions.first_seen, excluded.first_seen)`,
			env, c.Event, c.ContextKey, c.At.UnixMicro())
		if err != nil {
			return fmt.Errorf("record conversion: %w", err)
		}
	}
	return nil
}

// VariantCounts aggregates exposures of flag in env and the conversions on
// event that happened at or after each context's first exposure.
// Conversions before exposure cannot have been caused by the variant and
// are not attributed.
func (q *Queries) VariantCounts(ctx context.Context, env, flag, event string) ([]VariantCount, error) {
	rows, err := q.q.QueryContext(ctx, `
		SELECT e.variant,
		       COUNT(*),
		       COUNT(c.context_key)
		  FROM exposures e
		  LEFT JOIN conversions c
		         ON c.env_key = e.env_key
		        AND c.context_key = e.context_key
		        AND c.event_key = ?
		        AND c.first_seen >= e.first_seen
		 WHERE e.env_key = ? AND e.flag_key = ?
		 GROUP BY e.variant
		 ORDER BY e.variant`, event, env, flag)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VariantCount
	for rows.Next() {
		var vc VariantCount
		if err := rows.Scan(&vc.Variant, &vc.Exposed, &vc.Converted); err != nil {
			return nil, err
		}
		out = append(out, vc)
	}
	return out, rows.Err()
}

// ResetExperiment deletes the exposures recorded for a flag, which is
// needed when an experiment is restarted with a new salt.
func (q *Queries) ResetExperiment(ctx context.Context, env, flag string) (int64, error) {
	res, err := q.q.ExecContext(ctx, `DELETE FROM exposures WHERE env_key = ? AND flag_key = ?`, env, flag)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

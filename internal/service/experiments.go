package service

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/J-X0-cloud/flagpost/internal/stats"
	"github.com/J-X0-cloud/flagpost/internal/store"
	"github.com/J-X0-cloud/flagpost/model"
)

// EventResult reports how much of a batch was stored.
type EventResult struct {
	Accepted int `json:"accepted"`
	Rejected int `json:"rejected"`
}

// maxClockSkew bounds how far in the future an SDK timestamp may be before
// it is clamped to the server's clock.
const maxClockSkew = 5 * time.Minute

// RecordEvents stores an SDK event batch. Exposures for unknown flags or
// variants, and events without keys, are counted as rejected rather than
// failing the whole batch, because SDKs cannot do anything useful with a
// partial failure.
func (s *Service) RecordEvents(ctx context.Context, env string, b model.EventBatch, now time.Time) (EventResult, error) {
	var res EventResult
	if n := len(b.Exposures) + len(b.Conversions); n > model.MaxEventBatch {
		return res, fmt.Errorf("%w: batch has %d events, limit is %d", ErrInvalid, n, model.MaxEventBatch)
	}
	snap, err := s.Snapshot(ctx, env)
	if err != nil {
		return res, err
	}
	clamp := func(t time.Time) time.Time {
		if t.IsZero() || t.After(now.Add(maxClockSkew)) {
			return now
		}
		return t
	}
	var xs []store.Exposure
	for _, e := range b.Exposures {
		fs := snap.Flags[e.Flag]
		if fs == nil || e.ContextKey == "" {
			res.Rejected++
			continue
		}
		if _, ok := fs.Flag.Variant(e.Variant); !ok {
			res.Rejected++
			continue
		}
		xs = append(xs, store.Exposure{Flag: e.Flag, ContextKey: e.ContextKey, Variant: e.Variant, At: clamp(e.Timestamp)})
	}
	var cs []store.Conversion
	for _, c := range b.Conversions {
		if c.Event == "" || c.ContextKey == "" || !model.ValidKey(c.Event) {
			res.Rejected++
			continue
		}
		cs = append(cs, store.Conversion{Event: c.Event, ContextKey: c.ContextKey, At: clamp(c.Timestamp)})
	}
	if len(xs) == 0 && len(cs) == 0 {
		return res, nil
	}
	err = s.store.Tx(ctx, func(tx *store.Tx) error {
		if err := tx.RecordExposures(ctx, env, xs); err != nil {
			return err
		}
		return tx.RecordConversions(ctx, env, cs)
	})
	if err != nil {
		return res, err
	}
	res.Accepted = len(xs) + len(cs)
	return res, nil
}

// ExperimentResults is the analysis of one flag's experiment against one
// conversion metric.
type ExperimentResults struct {
	Environment string `json:"environment"`
	Flag        string `json:"flag"`
	Metric      string `json:"metric"`
	Control     string `json:"control"`
	stats.Analysis
}

// ExperimentResults analyses the exposures of flag in env against the
// conversion event metric. The configured weights of the flag's first
// experiment rollout are used for the sample ratio mismatch check, and the
// first variant of that rollout is the control unless one is given.
func (s *Service) ExperimentResults(ctx context.Context, env, flagKey, metric, control string, confidence float64) (ExperimentResults, error) {
	out := ExperimentResults{Environment: env, Flag: flagKey, Metric: metric}
	if !model.ValidKey(metric) {
		return out, fmt.Errorf("%w: metric must be an event key", ErrInvalid)
	}
	if confidence <= 0 || confidence >= 1 {
		return out, fmt.Errorf("%w: confidence must be between 0 and 1", ErrInvalid)
	}
	cfg, err := s.GetFlagConfig(ctx, env, flagKey)
	if err != nil {
		return out, err
	}
	rollout := experimentRollout(&cfg)
	counts, err := s.store.VariantCounts(ctx, env, flagKey, metric)
	if err != nil {
		return out, err
	}

	weights := map[string]float64{}
	var order []string
	if rollout != nil {
		for _, wv := range rollout.Variants {
			weights[wv.Variant] = float64(wv.Weight)
			order = append(order, wv.Variant)
		}
	}
	byVariant := map[string]store.VariantCount{}
	for _, c := range counts {
		byVariant[c.Variant] = c
		if !slices.Contains(order, c.Variant) {
			order = append(order, c.Variant)
		}
	}
	if control == "" && len(order) > 0 {
		control = order[0]
	}
	if control != "" && !slices.Contains(order, control) {
		return out, fmt.Errorf("%w: control variant %q has no data and is not in the experiment", ErrInvalid, control)
	}
	arms := make([]stats.Arm, 0, len(order))
	for _, v := range order {
		c := byVariant[v]
		arms = append(arms, stats.Arm{Variant: v, Weight: weights[v], Exposed: c.Exposed, Converted: c.Converted})
	}
	out.Control = control
	out.Analysis = stats.Analyze(arms, control, confidence)
	if rollout == nil {
		// Without configured weights the SRM test has nothing to compare.
		out.SRMPValue, out.SRMDetected = nil, false
	}
	return out, nil
}

// experimentRollout returns the first rollout marked as an experiment,
// checking rules in order and then the fallthrough.
func experimentRollout(c *model.FlagConfig) *model.Rollout {
	for _, r := range c.Rules {
		if r.Serve.Rollout != nil && r.Serve.Rollout.Experiment {
			return r.Serve.Rollout
		}
	}
	if c.Fallthrough.Rollout != nil && c.Fallthrough.Rollout.Experiment {
		return c.Fallthrough.Rollout
	}
	return nil
}

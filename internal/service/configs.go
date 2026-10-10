package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/J-X0-cloud/flagpost/internal/audit"
	"github.com/J-X0-cloud/flagpost/internal/store"
	"github.com/J-X0-cloud/flagpost/model"
)

// AnyVersion disables the optimistic concurrency check on a write.
const AnyVersion int64 = -1

// GetFlagConfig returns a flag's configuration in an environment, or the
// default configuration if it was never set.
func (s *Service) GetFlagConfig(ctx context.Context, env, flagKey string) (model.FlagConfig, error) {
	if _, err := s.store.GetEnvironment(ctx, env); err != nil {
		return model.FlagConfig{}, err
	}
	f, err := s.store.GetFlag(ctx, flagKey)
	if err != nil {
		return model.FlagConfig{}, err
	}
	return loadConfig(ctx, &s.store.Queries, &f, env)
}

func loadConfig(ctx context.Context, q *store.Queries, f *model.Flag, env string) (model.FlagConfig, error) {
	cfg, err := q.GetFlagConfig(ctx, f.Key, env)
	if errors.Is(err, store.ErrNotFound) {
		return model.DefaultConfig(f, env), nil
	}
	return cfg, err
}

// UpdateFlagConfig replaces a flag's configuration in an environment.
// Pass the version the change is based on (0 if the flag was never
// configured there), or AnyVersion to overwrite unconditionally. Rules
// without an ID are assigned one.
func (s *Service) UpdateFlagConfig(ctx context.Context, env, flagKey string, cfg model.FlagConfig, expectedVersion int64, comment string) (model.FlagConfig, error) {
	cfg.FlagKey, cfg.Environment = flagKey, env
	if cfg.Salt == "" {
		cfg.Salt = flagKey
	}
	for i := range cfg.Rules {
		if cfg.Rules[i].ID == "" {
			id, err := shortID()
			if err != nil {
				return cfg, err
			}
			cfg.Rules[i].ID = id
		}
	}
	return s.saveConfig(ctx, env, flagKey, audit.ActionUpdate, comment, expectedVersion, func(_ model.FlagConfig) (model.FlagConfig, error) {
		return cfg, nil
	})
}

// ToggleFlag turns a flag on or off in an environment, leaving the rest of
// its configuration alone.
func (s *Service) ToggleFlag(ctx context.Context, env, flagKey string, enabled bool, comment string) (model.FlagConfig, error) {
	return s.saveConfig(ctx, env, flagKey, audit.ActionToggle, comment, AnyVersion, func(cur model.FlagConfig) (model.FlagConfig, error) {
		cur.Enabled = enabled
		return cur, nil
	})
}

// saveConfig is the shared read-modify-write path for configuration
// changes.
func (s *Service) saveConfig(ctx context.Context, env, flagKey, action, comment string, expectedVersion int64,
	mutate func(model.FlagConfig) (model.FlagConfig, error)) (model.FlagConfig, error) {
	var saved model.FlagConfig
	err := s.write(ctx, func(tx *store.Tx, p *pending) error {
		if _, err := tx.GetEnvironment(ctx, env); err != nil {
			return err
		}
		f, err := tx.GetFlag(ctx, flagKey)
		if err != nil {
			return err
		}
		if f.Archived {
			return fmt.Errorf("%w: flag %q is archived", ErrInvalid, flagKey)
		}
		before, err := loadConfig(ctx, &tx.Queries, &f, env)
		if err != nil {
			return err
		}
		next, err := mutate(before)
		if err != nil {
			return err
		}
		next.FlagKey, next.Environment = flagKey, env
		if err := next.Validate(&f); err != nil {
			return err
		}
		if err := checkConfigRefs(ctx, tx, env, &next); err != nil {
			return err
		}
		if expectedVersion == AnyVersion {
			expectedVersion = before.Version
		}
		if saved, err = tx.PutFlagConfig(ctx, next, expectedVersion); err != nil {
			return err
		}
		var beforeDoc any = before
		if before.Version == 0 {
			beforeDoc = model.DefaultConfig(&f, env)
		}
		if err := recordAudit(ctx, tx, action, audit.ResourceFlagConfig, flagKey, env, comment, beforeDoc, saved); err != nil {
			return err
		}
		rev, err := tx.BumpRevision(ctx, env)
		if err != nil {
			return err
		}
		p.add(env, model.EventPatch, model.StreamPatch{Revision: rev, Flag: &model.FlagState{Flag: f, Config: saved}})
		return nil
	})
	return saved, err
}

// checkConfigRefs verifies that referenced segments exist, that each
// prerequisite names an existing flag and one of its variants, and that
// the prerequisite graph of the environment stays acyclic.
func checkConfigRefs(ctx context.Context, tx *store.Tx, env string, cfg *model.FlagConfig) error {
	var problems []model.Problem
	for _, key := range cfg.SegmentRefs() {
		if _, err := tx.GetSegment(ctx, env, key); errors.Is(err, store.ErrNotFound) {
			problems = append(problems, model.Problem{Field: "rules", Message: fmt.Sprintf("segment %q does not exist in %q", key, env)})
		} else if err != nil {
			return err
		}
	}
	for i, pr := range cfg.Prerequisites {
		field := fmt.Sprintf("prerequisites[%d]", i)
		pf, err := tx.GetFlag(ctx, pr.Flag)
		if errors.Is(err, store.ErrNotFound) {
			problems = append(problems, model.Problem{Field: field + ".flag", Message: fmt.Sprintf("flag %q does not exist", pr.Flag)})
			continue
		} else if err != nil {
			return err
		}
		if _, ok := pf.Variant(pr.Variant); !ok {
			problems = append(problems, model.Problem{Field: field + ".variant", Message: fmt.Sprintf("flag %q has no variant %q", pr.Flag, pr.Variant)})
		}
	}
	if len(problems) > 0 {
		return &model.ValidationError{Problems: problems}
	}
	if len(cfg.Prerequisites) == 0 {
		return nil
	}

	configs, err := tx.ListFlagConfigs(ctx, env)
	if err != nil {
		return err
	}
	graph := map[string][]string{}
	for k, c := range configs {
		for _, pr := range c.Prerequisites {
			graph[k] = append(graph[k], pr.Flag)
		}
	}
	graph[cfg.FlagKey] = nil
	for _, pr := range cfg.Prerequisites {
		graph[cfg.FlagKey] = append(graph[cfg.FlagKey], pr.Flag)
	}
	if cycle := findCycle(graph, cfg.FlagKey); cycle != nil {
		return &model.ValidationError{Problems: []model.Problem{{
			Field:   "prerequisites",
			Message: "creates a prerequisite cycle: " + strings.Join(cycle, " -> "),
		}}}
	}
	return nil
}

// findCycle runs a depth-first search from start and returns the first
// cycle reachable from it, as a path that begins and ends with the same
// node, or nil. Nodes are coloured white (unvisited), grey (on the current
// path) and black (finished); meeting a grey node closes a cycle.
func findCycle(graph map[string][]string, start string) []string {
	const (
		white = iota
		grey
		black
	)
	color := map[string]int{}
	var path []string
	var visit func(n string) []string
	visit = func(n string) []string {
		color[n] = grey
		path = append(path, n)
		for _, m := range graph[n] {
			switch color[m] {
			case grey:
				for i, p := range path {
					if p == m {
						return append(append([]string{}, path[i:]...), m)
					}
				}
			case white:
				if c := visit(m); c != nil {
					return c
				}
			}
		}
		path = path[:len(path)-1]
		color[n] = black
		return nil
	}
	return visit(start)
}

func shortID() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

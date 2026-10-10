package service

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/J-X0-cloud/flagpost/internal/audit"
	"github.com/J-X0-cloud/flagpost/internal/store"
	"github.com/J-X0-cloud/flagpost/model"
)

// ListFlags returns flag definitions.
func (s *Service) ListFlags(ctx context.Context, f store.FlagFilter) ([]model.Flag, error) {
	return s.store.ListFlags(ctx, f)
}

// GetFlag returns one flag definition.
func (s *Service) GetFlag(ctx context.Context, key string) (model.Flag, error) {
	return s.store.GetFlag(ctx, key)
}

// CreateFlag defines a new flag. It starts disabled in every environment.
func (s *Service) CreateFlag(ctx context.Context, f model.Flag) (model.Flag, error) {
	if err := f.Validate(); err != nil {
		return f, err
	}
	var created model.Flag
	err := s.write(ctx, func(tx *store.Tx, p *pending) error {
		var err error
		if created, err = tx.CreateFlag(ctx, f); err != nil {
			return err
		}
		if err := recordAudit(ctx, tx, audit.ActionCreate, audit.ResourceFlag, f.Key, "", "", nil, created); err != nil {
			return err
		}
		return s.broadcastFlag(ctx, tx, p, &created)
	})
	return created, err
}

// UpdateFlag replaces a flag definition. Changing a flag's kind is refused
// because SDK callers use typed accessors, and removing a variant is
// refused while any environment's configuration still serves it.
func (s *Service) UpdateFlag(ctx context.Context, f model.Flag, expectedVersion int64) (model.Flag, error) {
	if err := f.Validate(); err != nil {
		return f, err
	}
	var updated model.Flag
	err := s.write(ctx, func(tx *store.Tx, p *pending) error {
		before, err := tx.GetFlag(ctx, f.Key)
		if err != nil {
			return err
		}
		if before.Kind != f.Kind {
			return fmt.Errorf("%w: a flag's kind cannot change (it is %s)", ErrInvalid, before.Kind)
		}
		if err := checkVariantsStillServed(ctx, tx, &f); err != nil {
			return err
		}
		probe := f
		probe.Version, probe.CreatedAt, probe.UpdatedAt = before.Version, before.CreatedAt, before.UpdatedAt
		if same, err := unchanged(before, probe); err != nil || same {
			if same && before.Version != expectedVersion {
				return fmt.Errorf("flag %q: %w", f.Key, store.ErrConflict)
			}
			updated = before
			return err
		}
		if updated, err = tx.UpdateFlag(ctx, f, expectedVersion); err != nil {
			return err
		}
		if err := recordAudit(ctx, tx, audit.ActionUpdate, audit.ResourceFlag, f.Key, "", "", before, updated); err != nil {
			return err
		}
		return s.broadcastFlag(ctx, tx, p, &updated)
	})
	return updated, err
}

// checkVariantsStillServed rejects a definition that drops a variant some
// environment's configuration still refers to.
func checkVariantsStillServed(ctx context.Context, tx *store.Tx, f *model.Flag) error {
	envs, err := tx.ListEnvironments(ctx)
	if err != nil {
		return err
	}
	var problems []model.Problem
	for _, env := range envs {
		cfg, err := tx.GetFlagConfig(ctx, f.Key, env.Key)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return err
		}
		for _, v := range variantRefs(&cfg) {
			if _, ok := f.Variant(v); !ok {
				problems = append(problems, model.Problem{
					Field:   "variants",
					Message: fmt.Sprintf("variant %q is still served in environment %q", v, env.Key),
				})
			}
		}
	}
	if len(problems) > 0 {
		return &model.ValidationError{Problems: problems}
	}
	return nil
}

// variantRefs lists every variant a configuration can serve.
func variantRefs(c *model.FlagConfig) []string {
	var out []string
	addServe := func(sv model.Serve) {
		if sv.Rollout == nil {
			out = append(out, sv.Variant)
			return
		}
		for _, wv := range sv.Rollout.Variants {
			out = append(out, wv.Variant)
		}
	}
	out = append(out, c.OffVariant)
	addServe(c.Fallthrough)
	for _, t := range c.Targets {
		out = append(out, t.Variant)
	}
	for _, r := range c.Rules {
		addServe(r.Serve)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// DeleteFlag removes a flag everywhere. It is refused while another flag
// lists it as a prerequisite.
func (s *Service) DeleteFlag(ctx context.Context, key string) error {
	return s.write(ctx, func(tx *store.Tx, p *pending) error {
		before, err := tx.GetFlag(ctx, key)
		if err != nil {
			return err
		}
		deps, err := tx.FlagsDependingOn(ctx, key)
		if err != nil {
			return err
		}
		if len(deps) > 0 {
			return fmt.Errorf("%w: flag %q is a prerequisite of %s", ErrInUse, key, strings.Join(deps, ", "))
		}
		if err := tx.DeleteFlag(ctx, key); err != nil {
			return err
		}
		if err := recordAudit(ctx, tx, audit.ActionDelete, audit.ResourceFlag, key, "", "", before, nil); err != nil {
			return err
		}
		revs, err := tx.BumpAllRevisions(ctx)
		if err != nil {
			return err
		}
		for env, rev := range revs {
			p.add(env, model.EventDelete, model.StreamDelete{Revision: rev, Kind: model.ObjectFlag, Key: key})
		}
		return nil
	})
}

// broadcastFlag bumps every environment's revision after a definition
// change and queues the new flag state for each of them. Archived flags are
// removed from snapshots, so they are sent as deletes.
func (s *Service) broadcastFlag(ctx context.Context, tx *store.Tx, p *pending, f *model.Flag) error {
	revs, err := tx.BumpAllRevisions(ctx)
	if err != nil {
		return err
	}
	for env, rev := range revs {
		if f.Archived {
			p.add(env, model.EventDelete, model.StreamDelete{Revision: rev, Kind: model.ObjectFlag, Key: f.Key})
			continue
		}
		cfg, err := tx.GetFlagConfig(ctx, f.Key, env)
		if errors.Is(err, store.ErrNotFound) {
			cfg = model.DefaultConfig(f, env)
		} else if err != nil {
			return err
		}
		p.add(env, model.EventPatch, model.StreamPatch{Revision: rev, Flag: &model.FlagState{Flag: *f, Config: cfg}})
	}
	return nil
}

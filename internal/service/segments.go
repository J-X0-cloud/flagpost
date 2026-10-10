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

// ListSegments returns an environment's segments.
func (s *Service) ListSegments(ctx context.Context, env string) ([]model.Segment, error) {
	if _, err := s.store.GetEnvironment(ctx, env); err != nil {
		return nil, err
	}
	return s.store.ListSegments(ctx, env)
}

// GetSegment returns one segment.
func (s *Service) GetSegment(ctx context.Context, env, key string) (model.Segment, error) {
	return s.store.GetSegment(ctx, env, key)
}

// CreateSegment creates a segment in an environment.
func (s *Service) CreateSegment(ctx context.Context, seg model.Segment) (model.Segment, error) {
	if err := prepareSegment(&seg); err != nil {
		return seg, err
	}
	var created model.Segment
	err := s.write(ctx, func(tx *store.Tx, p *pending) error {
		if _, err := tx.GetEnvironment(ctx, seg.Environment); err != nil {
			return err
		}
		if err := checkSegmentRefs(ctx, tx, &seg); err != nil {
			return err
		}
		var err error
		if created, err = tx.CreateSegment(ctx, seg); err != nil {
			return err
		}
		if err := recordAudit(ctx, tx, audit.ActionCreate, audit.ResourceSegment, seg.Key, seg.Environment, "", nil, created); err != nil {
			return err
		}
		return queueSegment(ctx, tx, p, &created)
	})
	return created, err
}

// UpdateSegment replaces a segment if it is still at expectedVersion
// (or unconditionally with AnyVersion).
func (s *Service) UpdateSegment(ctx context.Context, seg model.Segment, expectedVersion int64) (model.Segment, error) {
	if err := prepareSegment(&seg); err != nil {
		return seg, err
	}
	var updated model.Segment
	err := s.write(ctx, func(tx *store.Tx, p *pending) error {
		before, err := tx.GetSegment(ctx, seg.Environment, seg.Key)
		if err != nil {
			return err
		}
		if err := checkSegmentRefs(ctx, tx, &seg); err != nil {
			return err
		}
		if expectedVersion == AnyVersion {
			expectedVersion = before.Version
		}
		probe := seg
		probe.Version, probe.CreatedAt, probe.UpdatedAt = before.Version, before.CreatedAt, before.UpdatedAt
		if same, err := unchanged(before, probe); err != nil || same {
			if same && before.Version != expectedVersion {
				return fmt.Errorf("segment %q: %w", seg.Key, store.ErrConflict)
			}
			updated = before
			return err
		}
		if updated, err = tx.UpdateSegment(ctx, seg, expectedVersion); err != nil {
			return err
		}
		if err := recordAudit(ctx, tx, audit.ActionUpdate, audit.ResourceSegment, seg.Key, seg.Environment, "", before, updated); err != nil {
			return err
		}
		return queueSegment(ctx, tx, p, &updated)
	})
	return updated, err
}

// DeleteSegment removes a segment unless a flag or another segment in the
// environment still references it.
func (s *Service) DeleteSegment(ctx context.Context, env, key string) error {
	return s.write(ctx, func(tx *store.Tx, p *pending) error {
		before, err := tx.GetSegment(ctx, env, key)
		if err != nil {
			return err
		}
		users, err := segmentUsers(ctx, tx, env, key)
		if err != nil {
			return err
		}
		if len(users) > 0 {
			return fmt.Errorf("%w: segment %q is referenced by %s", ErrInUse, key, strings.Join(users, ", "))
		}
		if err := tx.DeleteSegment(ctx, env, key); err != nil {
			return err
		}
		if err := recordAudit(ctx, tx, audit.ActionDelete, audit.ResourceSegment, key, env, "", before, nil); err != nil {
			return err
		}
		rev, err := tx.BumpRevision(ctx, env)
		if err != nil {
			return err
		}
		p.add(env, model.EventDelete, model.StreamDelete{Revision: rev, Kind: model.ObjectSegment, Key: key})
		return nil
	})
}

func prepareSegment(seg *model.Segment) error {
	for i := range seg.Rules {
		if seg.Rules[i].ID == "" {
			id, err := shortID()
			if err != nil {
				return err
			}
			seg.Rules[i].ID = id
		}
	}
	slices.Sort(seg.Included)
	seg.Included = slices.Compact(seg.Included)
	slices.Sort(seg.Excluded)
	seg.Excluded = slices.Compact(seg.Excluded)
	return seg.Validate()
}

func queueSegment(ctx context.Context, tx *store.Tx, p *pending, seg *model.Segment) error {
	rev, err := tx.BumpRevision(ctx, seg.Environment)
	if err != nil {
		return err
	}
	p.add(seg.Environment, model.EventPatch, model.StreamPatch{Revision: rev, Segment: seg})
	return nil
}

// checkSegmentRefs ensures nested segment references exist and do not
// form a cycle through the segment being saved.
func checkSegmentRefs(ctx context.Context, tx *store.Tx, seg *model.Segment) error {
	refs := seg.SegmentRefs()
	if len(refs) == 0 {
		return nil
	}
	all, err := tx.ListSegments(ctx, seg.Environment)
	if err != nil {
		return err
	}
	graph := map[string][]string{}
	for i := range all {
		graph[all[i].Key] = all[i].SegmentRefs()
	}
	var problems []model.Problem
	for _, r := range refs {
		if _, ok := graph[r]; !ok {
			problems = append(problems, model.Problem{Field: "rules", Message: fmt.Sprintf("segment %q does not exist", r)})
		}
	}
	if len(problems) > 0 {
		return &model.ValidationError{Problems: problems}
	}
	graph[seg.Key] = refs
	if cycle := findCycle(graph, seg.Key); cycle != nil {
		return &model.ValidationError{Problems: []model.Problem{{
			Field: "rules", Message: "creates a segment cycle: " + strings.Join(cycle, " -> "),
		}}}
	}
	return nil
}

// segmentUsers lists the flags and segments of env that reference key.
func segmentUsers(ctx context.Context, tx *store.Tx, env, key string) ([]string, error) {
	var users []string
	configs, err := tx.ListFlagConfigs(ctx, env)
	if err != nil {
		return nil, err
	}
	for flag, c := range configs {
		if slices.Contains(c.SegmentRefs(), key) {
			users = append(users, "flag "+flag)
		}
	}
	segs, err := tx.ListSegments(ctx, env)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	for i := range segs {
		if segs[i].Key != key && slices.Contains(segs[i].SegmentRefs(), key) {
			users = append(users, "segment "+segs[i].Key)
		}
	}
	slices.Sort(users)
	return users, nil
}

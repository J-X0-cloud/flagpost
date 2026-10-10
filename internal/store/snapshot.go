package store

import (
	"context"
	"errors"

	"github.com/J-X0-cloud/flagpost/model"
)

// ErrSnapshotUnstable is returned when concurrent writes kept changing the
// environment while a snapshot was being read.
var ErrSnapshotUnstable = errors.New("environment changed repeatedly while reading snapshot")

const snapshotAttempts = 5

// Snapshot assembles the complete evaluation state of an environment:
// every non-archived flag with its configuration (or the default one if it
// was never configured there) and every segment.
//
// The read is optimistic rather than transactional. All writes that affect
// an environment bump its revision in the same transaction, so if the
// revision is identical before and after reading the rows, no write was
// interleaved and the result is consistent. This keeps snapshot reads from
// taking the write lock that IMMEDIATE transactions would acquire.
func (q *Queries) Snapshot(ctx context.Context, env string) (*model.Snapshot, error) {
	for attempt := 0; attempt < snapshotAttempts; attempt++ {
		before, err := q.GetEnvironment(ctx, env)
		if err != nil {
			return nil, err
		}
		snap, err := q.readSnapshot(ctx, env, before.Revision)
		if err != nil {
			return nil, err
		}
		after, err := q.GetEnvironment(ctx, env)
		if err != nil {
			return nil, err
		}
		if after.Revision == before.Revision {
			return snap, nil
		}
	}
	return nil, ErrSnapshotUnstable
}

func (q *Queries) readSnapshot(ctx context.Context, env string, revision int64) (*model.Snapshot, error) {
	flags, err := q.ListFlags(ctx, FlagFilter{})
	if err != nil {
		return nil, err
	}
	configs, err := q.ListFlagConfigs(ctx, env)
	if err != nil {
		return nil, err
	}
	segs, err := q.ListSegments(ctx, env)
	if err != nil {
		return nil, err
	}
	snap := model.NewSnapshot(env, revision)
	for i := range flags {
		f := flags[i]
		cfg, ok := configs[f.Key]
		if !ok {
			cfg = model.DefaultConfig(&f, env)
		}
		snap.Flags[f.Key] = &model.FlagState{Flag: f, Config: cfg}
	}
	for i := range segs {
		s := segs[i]
		snap.Segments[s.Key] = &s
	}
	return snap, nil
}

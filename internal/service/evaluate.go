package service

import (
	"context"

	"github.com/J-X0-cloud/flagpost/eval"
	"github.com/J-X0-cloud/flagpost/model"
)

// Snapshot returns the current snapshot of env. Snapshots are cached per
// environment and revalidated against the stored revision with a single
// primary-key lookup, so repeated SDK polls and server-side evaluations do
// not re-read every flag. Callers must treat the result as immutable.
func (s *Service) Snapshot(ctx context.Context, env string) (*model.Snapshot, error) {
	c, err := s.cached(ctx, env)
	if err != nil {
		return nil, err
	}
	return c.snap, nil
}

// Evaluator returns an evaluator over the current snapshot of env.
func (s *Service) Evaluator(ctx context.Context, env string) (*eval.Evaluator, error) {
	c, err := s.cached(ctx, env)
	if err != nil {
		return nil, err
	}
	return c.ev, nil
}

func (s *Service) cached(ctx context.Context, env string) (*cachedEnv, error) {
	e, err := s.store.GetEnvironment(ctx, env)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	c := s.cache[env]
	s.mu.Unlock()
	if c != nil && c.snap.Revision == e.Revision {
		return c, nil
	}
	snap, err := s.store.Snapshot(ctx, env)
	if err != nil {
		return nil, err
	}
	c = &cachedEnv{snap: snap, ev: eval.New(snap)}
	s.mu.Lock()
	// Another goroutine may have loaded a newer revision meanwhile.
	if cur := s.cache[env]; cur == nil || cur.snap.Revision < snap.Revision {
		s.cache[env] = c
	}
	s.mu.Unlock()
	return c, nil
}

// Evaluate evaluates one flag in env for ctx. With explain set the result
// carries a decision trace.
func (s *Service) Evaluate(ctx context.Context, env, flagKey string, ec eval.Context, explain bool) (eval.Result, error) {
	ev, err := s.Evaluator(ctx, env)
	if err != nil {
		return eval.Result{}, err
	}
	if explain {
		return ev.Explain(flagKey, ec), nil
	}
	return ev.Evaluate(flagKey, ec), nil
}

// EvaluateAll evaluates every flag in env for ctx.
func (s *Service) EvaluateAll(ctx context.Context, env string, ec eval.Context) ([]eval.Result, error) {
	ev, err := s.Evaluator(ctx, env)
	if err != nil {
		return nil, err
	}
	return ev.EvaluateAll(ec), nil
}

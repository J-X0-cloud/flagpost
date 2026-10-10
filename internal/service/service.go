// Package service implements flagpost's business logic on top of the
// store: validation that spans several objects (segment references,
// prerequisite cycles, variants still in use), audit logging with diffs,
// environment revision bookkeeping, change publication to the stream
// broker, and cached snapshots for evaluation.
//
// Every mutating method runs in a single store transaction that also writes
// the audit entry and bumps the affected revisions; stream events are
// published only after that transaction commits, so subscribers never see a
// change that was rolled back.
package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/J-X0-cloud/flagpost/eval"
	"github.com/J-X0-cloud/flagpost/internal/audit"
	"github.com/J-X0-cloud/flagpost/internal/store"
	"github.com/J-X0-cloud/flagpost/internal/stream"
	"github.com/J-X0-cloud/flagpost/model"
)

// Errors returned in addition to the store's sentinel errors.
var (
	// ErrInUse means the object is referenced by others and cannot be
	// removed or changed in the requested way.
	ErrInUse = errors.New("in use")
	// ErrInvalid means the request is malformed in a way that is not a
	// field-level validation problem.
	ErrInvalid = errors.New("invalid request")
)

type actorKey struct{}

// WithActor records who is making changes, for the audit log.
func WithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

// Actor returns the actor stored in ctx, or "system".
func Actor(ctx context.Context) string {
	if a, ok := ctx.Value(actorKey{}).(string); ok && a != "" {
		return a
	}
	return "system"
}

// Service is safe for concurrent use.
type Service struct {
	store  *store.Store
	broker *stream.Broker
	log    *slog.Logger

	mu    sync.Mutex
	cache map[string]*cachedEnv
}

type cachedEnv struct {
	snap *model.Snapshot
	ev   *eval.Evaluator
}

// New returns a Service. A nil logger discards logs.
func New(st *store.Store, broker *stream.Broker, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{store: st, broker: broker, log: log, cache: map[string]*cachedEnv{}}
}

// Store exposes the underlying store for read paths that need no logic.
func (s *Service) Store() *store.Store { return s.store }

// Broker exposes the stream broker.
func (s *Service) Broker() *stream.Broker { return s.broker }

// pending collects stream events during a transaction.
type pending struct {
	events []pendingEvent
}

type pendingEvent struct {
	env, typ string
	payload  any
}

func (p *pending) add(env, typ string, payload any) {
	p.events = append(p.events, pendingEvent{env, typ, payload})
}

// publish delivers collected events after commit.
func (s *Service) publish(p *pending) {
	for _, e := range p.events {
		data, err := json.Marshal(e.payload)
		if err != nil {
			s.log.Error("encode stream event", "env", e.env, "type", e.typ, "err", err)
			continue
		}
		s.broker.Publish(e.env, e.typ, data)
	}
}

// write runs fn in a transaction and publishes its events on success.
func (s *Service) write(ctx context.Context, fn func(tx *store.Tx, p *pending) error) error {
	p := &pending{}
	if err := s.store.Tx(ctx, func(tx *store.Tx) error { return fn(tx, p) }); err != nil {
		return err
	}
	s.publish(p)
	return nil
}

func recordAudit(ctx context.Context, tx *store.Tx, action, resourceType, key, env, comment string, before, after any) error {
	changes, err := audit.Diff(before, after, audit.DiffOptions{Ignore: []string{"version", "updatedAt", "createdAt", "revision"}})
	if err != nil {
		return err
	}
	if action == audit.ActionUpdate && len(changes) == 0 && comment == "" {
		return nil // no-op saves do not clutter the log
	}
	var b, a json.RawMessage
	if before != nil {
		if b, err = json.Marshal(before); err != nil {
			return err
		}
	}
	if after != nil {
		if a, err = json.Marshal(after); err != nil {
			return err
		}
	}
	_, err = tx.InsertAudit(ctx, audit.Entry{
		Actor: Actor(ctx), Action: action, ResourceType: resourceType, ResourceKey: key,
		Environment: env, Comment: comment, Before: b, After: a, Changes: changes,
	})
	return err
}

func newSDKKey() (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "fp-sdk-" + hex.EncodeToString(b[:]), nil
}

// ---------------------------------------------------------------------------
// Environments

// ListEnvironments returns all environments.
func (s *Service) ListEnvironments(ctx context.Context) ([]model.Environment, error) {
	return s.store.ListEnvironments(ctx)
}

// GetEnvironment returns one environment.
func (s *Service) GetEnvironment(ctx context.Context, key string) (model.Environment, error) {
	return s.store.GetEnvironment(ctx, key)
}

// EnvironmentForSDKKey authenticates an SDK key.
func (s *Service) EnvironmentForSDKKey(ctx context.Context, sdkKey string) (model.Environment, error) {
	if sdkKey == "" {
		return model.Environment{}, store.ErrNotFound
	}
	return s.store.GetEnvironmentBySDKKey(ctx, sdkKey)
}

// CreateEnvironment creates an environment with a fresh SDK key.
func (s *Service) CreateEnvironment(ctx context.Context, e model.Environment) (model.Environment, error) {
	if err := e.Validate(); err != nil {
		return e, err
	}
	key, err := newSDKKey()
	if err != nil {
		return e, err
	}
	e.SDKKey = key
	var created model.Environment
	err = s.write(ctx, func(tx *store.Tx, _ *pending) error {
		var err error
		if created, err = tx.CreateEnvironment(ctx, e); err != nil {
			return err
		}
		return recordAudit(ctx, tx, audit.ActionCreate, audit.ResourceEnvironment, e.Key, e.Key, "", nil, redactEnv(created))
	})
	return created, err
}

// UpdateEnvironment changes an environment's name and description.
func (s *Service) UpdateEnvironment(ctx context.Context, key, name, description string) (model.Environment, error) {
	probe := model.Environment{Key: key, Name: name}
	if err := probe.Validate(); err != nil {
		return probe, err
	}
	var updated model.Environment
	err := s.write(ctx, func(tx *store.Tx, _ *pending) error {
		before, err := tx.GetEnvironment(ctx, key)
		if err != nil {
			return err
		}
		if updated, err = tx.UpdateEnvironment(ctx, key, name, description); err != nil {
			return err
		}
		return recordAudit(ctx, tx, audit.ActionUpdate, audit.ResourceEnvironment, key, key, "", redactEnv(before), redactEnv(updated))
	})
	return updated, err
}

// RotateSDKKey issues a new SDK key for an environment. The old key stops
// working immediately; connected streams are not dropped.
func (s *Service) RotateSDKKey(ctx context.Context, key string) (model.Environment, error) {
	sdkKey, err := newSDKKey()
	if err != nil {
		return model.Environment{}, err
	}
	var env model.Environment
	err = s.write(ctx, func(tx *store.Tx, _ *pending) error {
		if err := tx.SetSDKKey(ctx, key, sdkKey); err != nil {
			return err
		}
		var err error
		if env, err = tx.GetEnvironment(ctx, key); err != nil {
			return err
		}
		_, err = tx.InsertAudit(ctx, audit.Entry{
			Actor: Actor(ctx), Action: audit.ActionRotateKey, ResourceType: audit.ResourceEnvironment,
			ResourceKey: key, Environment: key,
		})
		return err
	})
	return env, err
}

// DeleteEnvironment removes an environment and everything scoped to it.
func (s *Service) DeleteEnvironment(ctx context.Context, key string) error {
	err := s.write(ctx, func(tx *store.Tx, _ *pending) error {
		before, err := tx.GetEnvironment(ctx, key)
		if err != nil {
			return err
		}
		if err := tx.DeleteEnvironment(ctx, key); err != nil {
			return err
		}
		return recordAudit(ctx, tx, audit.ActionDelete, audit.ResourceEnvironment, key, key, "", redactEnv(before), nil)
	})
	if err == nil {
		s.mu.Lock()
		delete(s.cache, key)
		s.mu.Unlock()
	}
	return err
}

// redactEnv keeps SDK keys out of the audit log.
func redactEnv(e model.Environment) model.Environment {
	e.SDKKey = ""
	return e
}

// ---------------------------------------------------------------------------
// Audit

// ListAudit returns audit entries newest first.
func (s *Service) ListAudit(ctx context.Context, f audit.Filter) ([]audit.Entry, error) {
	return s.store.ListAudit(ctx, f)
}

func wrapf(err error, format string, args ...any) error {
	return fmt.Errorf("%s: %w", fmt.Sprintf(format, args...), err)
}

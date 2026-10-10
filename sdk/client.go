// Package sdk is the Go client for flagpost. It keeps a local copy of an
// environment's flags and segments, refreshed over a server-sent event
// stream (or by polling), and evaluates flags in-process with the same
// engine the server uses. Evaluation therefore costs no network round trip
// and keeps working, with the last known configuration, while the server is
// unreachable.
//
//	client, err := sdk.New(sdk.Config{BaseURL: "http://localhost:8080", SDKKey: key})
//	if err != nil { ... }
//	defer client.Close()
//	_ = client.WaitReady(ctx)
//	if client.Bool("new-checkout", eval.NewContext(userID), false) { ... }
package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/J-X0-cloud/flagpost/eval"
	"github.com/J-X0-cloud/flagpost/model"
)

// Mode selects how the client receives updates.
type Mode int

// Update modes.
const (
	// Streaming holds a server-sent event connection open and applies
	// changes as they happen. It is the default.
	Streaming Mode = iota
	// Polling fetches the snapshot periodically, using ETags so that an
	// unchanged environment costs a 304.
	Polling
)

// Config configures a Client.
type Config struct {
	// BaseURL is the flagpost server, for example "http://flagpost:8080".
	BaseURL string
	// SDKKey identifies the environment.
	SDKKey string
	// Mode selects streaming or polling. Defaults to Streaming.
	Mode Mode
	// PollInterval is the polling period. Defaults to 30 seconds.
	PollInterval time.Duration
	// HTTPClient is used for every request. It must not set a global
	// Timeout when streaming, since streams are long-lived. Defaults to a
	// client without timeout; per-request deadlines are applied
	// internally.
	HTTPClient *http.Client
	// StreamIdleTimeout reconnects a stream that has received nothing,
	// not even a heartbeat, for this long. Defaults to 75 seconds.
	StreamIdleTimeout time.Duration
	// CacheFile, if set, persists every snapshot received to this path
	// and loads it at startup, so a process that starts while the server
	// is down can still serve the last known configuration.
	CacheFile string
	// DisableEvents turns off exposure and conversion reporting.
	DisableEvents bool
	// FlushInterval is how often buffered events are sent. Defaults to
	// 10 seconds.
	FlushInterval time.Duration
	// EventCapacity bounds the number of buffered events. Defaults to
	// 10000.
	EventCapacity int
	// OnUpdate, if set, is called after every snapshot change with the
	// new snapshot. It runs on the update goroutine and must not block.
	OnUpdate func(*model.Snapshot)
	// Logger receives diagnostics. Defaults to discarding them.
	Logger *slog.Logger
}

// Errors reported by WaitReady.
var (
	ErrUnauthorized = errors.New("flagpost: SDK key rejected by server")
	ErrClosed       = errors.New("flagpost: client is closed")
)

type state struct {
	snap *model.Snapshot
	ev   *eval.Evaluator
}

// Client evaluates flags locally. It is safe for concurrent use.
type Client struct {
	cfg    Config
	state  atomic.Pointer[state]
	events *eventProcessor

	ready     chan struct{}
	readyOnce sync.Once
	fatal     atomic.Pointer[error]

	cancel context.CancelFunc
	wg     sync.WaitGroup
	closed atomic.Bool
}

// New starts a client. It returns immediately; use WaitReady to block
// until the first snapshot has arrived.
func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("flagpost: BaseURL is required")
	}
	if cfg.SDKKey == "" {
		return nil, errors.New("flagpost: SDKKey is required")
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{}
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 30 * time.Second
	}
	if cfg.StreamIdleTimeout <= 0 {
		cfg.StreamIdleTimeout = 75 * time.Second
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = 10 * time.Second
	}
	if cfg.EventCapacity <= 0 {
		cfg.EventCapacity = 10000
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	c := &Client{cfg: cfg, ready: make(chan struct{})}
	c.events = newEventProcessor(cfg.EventCapacity, cfg.EventCapacity, c.postEvents, cfg.Logger, time.Now)

	if cfg.CacheFile != "" {
		if snap, err := readCache(cfg.CacheFile); err == nil {
			c.setSnapshot(snap, false)
			cfg.Logger.Info("flagpost: loaded cached snapshot", "revision", snap.Revision)
		} else if !errors.Is(err, os.ErrNotExist) {
			cfg.Logger.Warn("flagpost: ignoring unreadable cache file", "path", cfg.CacheFile, "err", err)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		if cfg.Mode == Polling {
			c.pollLoop(ctx)
		} else {
			c.streamLoop(ctx)
		}
	}()
	if !cfg.DisableEvents {
		c.wg.Add(1)
		go func() {
			defer c.wg.Done()
			c.flushLoop(ctx)
		}()
	}
	return c, nil
}

// WaitReady blocks until the client holds a snapshot (from the server or
// the cache file), the server rejects the SDK key, or ctx ends.
func (c *Client) WaitReady(ctx context.Context) error {
	select {
	case <-c.ready:
		if errp := c.fatal.Load(); errp != nil && c.state.Load() == nil {
			return *errp
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Ready reports whether the client holds a snapshot.
func (c *Client) Ready() bool { return c.state.Load() != nil }

// Snapshot returns the snapshot currently in use, or nil before the client
// is ready. The result must not be modified.
func (c *Client) Snapshot() *model.Snapshot {
	if st := c.state.Load(); st != nil {
		return st.snap
	}
	return nil
}

func (c *Client) markReady() { c.readyOnce.Do(func() { close(c.ready) }) }

func (c *Client) setFatal(err error) {
	c.fatal.Store(&err)
	c.markReady()
}

// setSnapshot installs a new snapshot, ignoring ones older than the
// current revision so that a slow poll cannot roll back a stream update.
func (c *Client) setSnapshot(snap *model.Snapshot, persist bool) {
	for {
		cur := c.state.Load()
		if cur != nil && snap.Revision < cur.snap.Revision {
			return
		}
		if c.state.CompareAndSwap(cur, &state{snap: snap, ev: eval.New(snap)}) {
			break
		}
	}
	c.markReady()
	if persist && c.cfg.CacheFile != "" {
		if err := writeCache(c.cfg.CacheFile, snap); err != nil {
			c.cfg.Logger.Warn("flagpost: could not write cache file", "path", c.cfg.CacheFile, "err", err)
		}
	}
	if c.cfg.OnUpdate != nil {
		c.cfg.OnUpdate(snap)
	}
}

// update applies fn to the current snapshot.
func (c *Client) update(fn func(*model.Snapshot) *model.Snapshot) {
	cur := c.state.Load()
	if cur == nil {
		return // patches before the first put carry nothing to patch
	}
	if next := fn(cur.snap); next != cur.snap {
		c.setSnapshot(next, true)
	}
}

// Evaluate evaluates a flag and returns the full result, including the
// reason. Results for experiment rollouts are reported as exposures.
func (c *Client) Evaluate(flagKey string, ctx eval.Context) eval.Result {
	st := c.state.Load()
	if st == nil {
		return eval.Result{FlagKey: flagKey, Reason: eval.ReasonError, RuleIndex: -1,
			Error: eval.ErrClientNotReady, Message: "no snapshot received yet"}
	}
	res := st.ev.Evaluate(flagKey, ctx)
	if res.InExperiment && !c.cfg.DisableEvents {
		c.events.exposure(flagKey, res.Variant, ctx.Key)
	}
	return res
}

// AllFlags evaluates every flag for ctx without recording exposures, for
// bootstrapping front-end clients.
func (c *Client) AllFlags(ctx eval.Context) []eval.Result {
	st := c.state.Load()
	if st == nil {
		return nil
	}
	return st.ev.EvaluateAll(ctx)
}

// Bool returns a boolean flag's value, or def if it cannot be evaluated or
// is not a boolean.
func (c *Client) Bool(flagKey string, ctx eval.Context, def bool) bool {
	var v bool
	if c.decode(flagKey, ctx, &v) != nil {
		return def
	}
	return v
}

// String returns a string flag's value, or def.
func (c *Client) String(flagKey string, ctx eval.Context, def string) string {
	var v string
	if c.decode(flagKey, ctx, &v) != nil {
		return def
	}
	return v
}

// Float64 returns a number flag's value, or def.
func (c *Client) Float64(flagKey string, ctx eval.Context, def float64) float64 {
	var v float64
	if c.decode(flagKey, ctx, &v) != nil {
		return def
	}
	return v
}

// JSON decodes a flag's value into out. On error out is left untouched
// and the error describes why, so the caller can apply its default.
func (c *Client) JSON(flagKey string, ctx eval.Context, out any) error {
	return c.decode(flagKey, ctx, out)
}

// EvaluationError is returned by JSON when evaluation fails.
type EvaluationError struct {
	Kind    eval.ErrorKind
	Message string
}

func (e *EvaluationError) Error() string { return fmt.Sprintf("flagpost: %s: %s", e.Kind, e.Message) }

func (c *Client) decode(flagKey string, ctx eval.Context, out any) error {
	res := c.Evaluate(flagKey, ctx)
	if res.Failed() {
		return &EvaluationError{Kind: res.Error, Message: res.Message}
	}
	if err := json.Unmarshal(res.Value, out); err != nil {
		return &EvaluationError{Kind: eval.ErrWrongType, Message: err.Error()}
	}
	return nil
}

// Track records a conversion event for ctx, for experiment analysis.
func (c *Client) Track(event string, ctx eval.Context) {
	if !c.cfg.DisableEvents {
		c.events.conversion(event, ctx.Key)
	}
}

// Flush sends buffered events now.
func (c *Client) Flush(ctx context.Context) error {
	if c.cfg.DisableEvents {
		return nil
	}
	return c.events.flush(ctx)
}

// Close stops background work and makes a final attempt to deliver
// buffered events, waiting at most five seconds.
func (c *Client) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	c.cancel()
	c.wg.Wait()
	if c.cfg.DisableEvents {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.events.flush(ctx)
}

func (c *Client) flushLoop(ctx context.Context) {
	t := time.NewTicker(c.cfg.FlushInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			if err := c.events.flush(fctx); err != nil && ctx.Err() == nil {
				c.cfg.Logger.Warn("flagpost: event flush failed; will retry", "err", err)
			}
			cancel()
		}
	}
}

func (c *Client) authorize(req *http.Request) {
	req.Header.Set("Authorization", c.cfg.SDKKey)
	req.Header.Set("User-Agent", "flagpost-go-sdk")
}

// backoff computes reconnection delays: exponential growth from base to
// max with "full jitter", which spreads reconnecting clients out after a
// server restart instead of having them return in lockstep.
type backoff struct {
	base, max time.Duration
	attempt   int
}

func (b *backoff) next() time.Duration {
	d := b.base << min(b.attempt, 20)
	if d > b.max || d <= 0 {
		d = b.max
	}
	b.attempt++
	return time.Duration(rand.Int64N(int64(d)) + 1)
}

func (b *backoff) reset() { b.attempt = 0 }

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func readCache(path string) (*model.Snapshot, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var snap model.Snapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		return nil, err
	}
	if snap.Flags == nil {
		snap.Flags = map[string]*model.FlagState{}
	}
	if snap.Segments == nil {
		snap.Segments = map[string]*model.Segment{}
	}
	return &snap, nil
}

// writeCache writes atomically: a crash mid-write leaves the previous
// cache intact rather than a truncated file.
func writeCache(path string, snap *model.Snapshot) error {
	b, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".flagpost-cache-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func randFloat() float64 { return rand.Float64() }

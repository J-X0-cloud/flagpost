package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/J-X0-cloud/flagpost/eval"
	"github.com/J-X0-cloud/flagpost/internal/api"
	"github.com/J-X0-cloud/flagpost/internal/service"
	"github.com/J-X0-cloud/flagpost/internal/store"
	"github.com/J-X0-cloud/flagpost/internal/stream"
	"github.com/J-X0-cloud/flagpost/model"
)

// server runs a real flagpost server for end-to-end SDK tests.
type server struct {
	t      *testing.T
	svc    *service.Service
	http   *httptest.Server
	sdkKey string
	ctx    context.Context
}

func newServer(t *testing.T) *server {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "sdk.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	broker := stream.NewBroker(stream.Options{})
	svc := service.New(st, broker, nil)
	h := api.New(svc, api.Config{AdminToken: "admin", Heartbeat: 100 * time.Millisecond})
	hs := httptest.NewServer(h.Handler())
	s := &server{t: t, svc: svc, http: hs, ctx: service.WithActor(context.Background(), "test")}
	t.Cleanup(func() {
		broker.Shutdown()
		hs.Close()
		st.Close()
	})
	env, err := svc.CreateEnvironment(s.ctx, model.Environment{Key: "prod", Name: "Production"})
	if err != nil {
		t.Fatal(err)
	}
	s.sdkKey = env.SDKKey
	return s
}

func (s *server) createFlag(f model.Flag) {
	s.t.Helper()
	if _, err := s.svc.CreateFlag(s.ctx, f); err != nil {
		s.t.Fatal(err)
	}
}

func (s *server) configure(flag string, mutate func(*model.FlagConfig)) {
	s.t.Helper()
	cfg, err := s.svc.GetFlagConfig(s.ctx, "prod", flag)
	if err != nil {
		s.t.Fatal(err)
	}
	mutate(&cfg)
	if _, err := s.svc.UpdateFlagConfig(s.ctx, "prod", flag, cfg, service.AnyVersion, ""); err != nil {
		s.t.Fatal(err)
	}
}

func boolFlag(key string) model.Flag {
	return model.Flag{Key: key, Name: key, Kind: model.KindBoolean, Variants: []model.Variant{
		{Key: "on", Value: json.RawMessage(`true`)},
		{Key: "off", Value: json.RawMessage(`false`)},
	}}
}

func (s *server) client(mutate func(*Config)) *Client {
	s.t.Helper()
	cfg := Config{BaseURL: s.http.URL, SDKKey: s.sdkKey, FlushInterval: time.Hour, PollInterval: 50 * time.Millisecond}
	if mutate != nil {
		mutate(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.WaitReady(ctx); err != nil {
		s.t.Fatalf("client not ready: %v", err)
	}
	return c
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestNewValidatesConfig(t *testing.T) {
	if _, err := New(Config{SDKKey: "k"}); err == nil {
		t.Error("missing BaseURL accepted")
	}
	if _, err := New(Config{BaseURL: "http://x"}); err == nil {
		t.Error("missing SDKKey accepted")
	}
}

func TestClientNotReady(t *testing.T) {
	c, err := New(Config{BaseURL: "http://127.0.0.1:1", SDKKey: "k", DisableEvents: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Ready() || c.Snapshot() != nil || c.AllFlags(eval.Context{}) != nil {
		t.Error("client should not be ready")
	}
	res := c.Evaluate("f", eval.NewContext("u"))
	if res.Error != eval.ErrClientNotReady {
		t.Errorf("result %+v", res)
	}
	if !c.Bool("f", eval.NewContext("u"), true) {
		t.Error("default not returned")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := c.WaitReady(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("WaitReady = %v", err)
	}
}

func TestStreamingClient(t *testing.T) {
	s := newServer(t)
	s.createFlag(boolFlag("checkout"))
	s.createFlag(model.Flag{Key: "theme", Name: "Theme", Kind: model.KindString, Variants: []model.Variant{
		{Key: "light", Value: json.RawMessage(`"light"`)},
		{Key: "dark", Value: json.RawMessage(`"dark"`)},
	}})
	var updates atomic.Int32
	c := s.client(func(cfg *Config) { cfg.OnUpdate = func(*model.Snapshot) { updates.Add(1) } })
	user := eval.NewContext("u1").With("plan", "pro")

	if c.Bool("checkout", user, true) {
		t.Error("checkout should start off")
	}
	s.configure("checkout", func(cfg *model.FlagConfig) {
		cfg.Enabled = true
		cfg.Fallthrough = model.Serve{Variant: "off"}
		cfg.Rules = []model.Rule{{ID: "pro", Clauses: []model.Clause{{Attribute: "plan", Operator: model.OpEquals, Values: []any{"pro"}}}, Serve: model.Serve{Variant: "on"}}}
	})
	eventually(t, "checkout to turn on", func() bool { return c.Bool("checkout", user, false) })
	if c.Bool("checkout", eval.NewContext("u2"), true) {
		t.Error("free user should fall through to off")
	}

	if got := c.String("theme", user, "x"); got != "dark" {
		t.Errorf("theme = %q (off variant should be the last one)", got)
	}
	if got := c.Float64("theme", user, 1.5); got != 1.5 {
		t.Errorf("wrong-type access should return the default, got %v", got)
	}
	var out map[string]any
	err := c.JSON("missing", user, &out)
	var ee *EvaluationError
	if !errors.As(err, &ee) || ee.Kind != eval.ErrFlagNotFound {
		t.Errorf("JSON on missing flag: %v", err)
	}

	if err := s.svc.DeleteFlag(s.ctx, "theme"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "theme deletion", func() bool { return c.Evaluate("theme", user).Error == eval.ErrFlagNotFound })
	if len(c.AllFlags(user)) != 1 || updates.Load() < 3 {
		t.Errorf("all flags %d, updates %d", len(c.AllFlags(user)), updates.Load())
	}
}

func TestStreamingClientSegmentsAndReconnect(t *testing.T) {
	s := newServer(t)
	s.createFlag(boolFlag("beta-ui"))
	if _, err := s.svc.CreateSegment(s.ctx, model.Segment{Environment: "prod", Key: "beta", Name: "Beta", Included: []string{"u1"}}); err != nil {
		t.Fatal(err)
	}
	s.configure("beta-ui", func(cfg *model.FlagConfig) {
		cfg.Enabled = true
		cfg.Fallthrough = model.Serve{Variant: "off"}
		cfg.Rules = []model.Rule{{ID: "b", Clauses: []model.Clause{{Operator: model.OpInSegment, Values: []any{"beta"}}}, Serve: model.Serve{Variant: "on"}}}
	})

	// A proxy in front of the server that we can cut, to force reconnects.
	var connections atomic.Int32
	var cut atomic.Bool
	target := s.http.URL
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connections.Add(1)
		if cut.Load() {
			http.Error(w, "unavailable", http.StatusBadGateway)
			return
		}
		req, _ := http.NewRequestWithContext(r.Context(), r.Method, target+r.URL.RequestURI(), r.Body)
		req.Header = r.Header.Clone()
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer res.Body.Close()
		for k, v := range res.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(res.StatusCode)
		buf := make([]byte, 4096)
		for {
			n, err := res.Body.Read(buf)
			if cut.Load() {
				return // drop the connection without forwarding anything more
			}
			if n > 0 {
				if _, werr := w.Write(buf[:n]); werr != nil {
					return
				}
				w.(http.Flusher).Flush()
			}
			if err != nil {
				return
			}
		}
	}))
	t.Cleanup(proxy.Close) // registered before the client, so it runs after the client closes

	c := s.client(func(cfg *Config) {
		cfg.BaseURL = proxy.URL
		cfg.StreamIdleTimeout = 300 * time.Millisecond
	})
	if !c.Bool("beta-ui", eval.NewContext("u1"), false) {
		t.Fatal("segment member should see the flag")
	}

	cut.Store(true)
	if _, err := s.svc.UpdateSegment(s.ctx, model.Segment{Environment: "prod", Key: "beta", Name: "Beta", Included: []string{"u2"}}, service.AnyVersion); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond) // the client keeps serving the last known state
	if !c.Bool("beta-ui", eval.NewContext("u1"), false) {
		t.Error("client lost its snapshot during the outage")
	}
	cut.Store(false)
	eventually(t, "segment update after reconnect", func() bool {
		return c.Bool("beta-ui", eval.NewContext("u2"), false) && !c.Bool("beta-ui", eval.NewContext("u1"), true)
	})
	if connections.Load() < 2 {
		t.Errorf("expected a reconnect, saw %d connections", connections.Load())
	}
}

func TestPollingClient(t *testing.T) {
	s := newServer(t)
	s.createFlag(boolFlag("checkout"))
	c := s.client(func(cfg *Config) { cfg.Mode = Polling })
	if c.Bool("checkout", eval.NewContext("u"), true) {
		t.Fatal("should start off")
	}
	s.configure("checkout", func(cfg *model.FlagConfig) { cfg.Enabled = true })
	eventually(t, "poll to pick up change", func() bool { return c.Bool("checkout", eval.NewContext("u"), false) })
}

func TestUnauthorized(t *testing.T) {
	s := newServer(t)
	for _, mode := range []Mode{Streaming, Polling} {
		c, err := New(Config{BaseURL: s.http.URL, SDKKey: "wrong", Mode: mode, DisableEvents: true})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := c.WaitReady(ctx); !errors.Is(err, ErrUnauthorized) {
			t.Errorf("mode %d: WaitReady = %v", mode, err)
		}
		cancel()
		c.Close()
	}
}

func TestExperimentEventsReachServer(t *testing.T) {
	s := newServer(t)
	s.createFlag(boolFlag("exp"))
	s.configure("exp", func(cfg *model.FlagConfig) {
		cfg.Enabled = true
		cfg.Fallthrough = model.Serve{Rollout: &model.Rollout{Experiment: true, Variants: []model.WeightedVariant{
			{Variant: "off", Weight: 50000}, {Variant: "on", Weight: 50000},
		}}}
	})
	c := s.client(nil)
	for i := 0; i < 100; i++ {
		ctx := eval.NewContext("user-" + strings.Repeat("x", i%7) + string(rune('a'+i%26)) + string(rune('a'+i/26)))
		c.Bool("exp", ctx, false)
		c.Bool("exp", ctx, false) // duplicate exposure is deduplicated
		if i%3 == 0 {
			c.Track("purchase", ctx)
		}
	}
	if err := c.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	res, err := s.svc.ExperimentResults(s.ctx, "prod", "exp", "purchase", "", 0.95)
	if err != nil {
		t.Fatal(err)
	}
	var exposed, converted int64
	for _, a := range res.Arms {
		exposed += a.Exposed
		converted += a.Converted
	}
	if exposed != 100 || converted != 34 {
		t.Errorf("server saw %d exposures and %d conversions", exposed, converted)
	}
}

func TestCacheFileFallback(t *testing.T) {
	s := newServer(t)
	s.createFlag(boolFlag("checkout"))
	s.configure("checkout", func(cfg *model.FlagConfig) { cfg.Enabled = true })
	cache := filepath.Join(t.TempDir(), "flags.json")
	c := s.client(func(cfg *Config) { cfg.CacheFile = cache })
	eventually(t, "cache file", func() bool {
		_, err := os.Stat(cache)
		return err == nil
	})
	c.Close()

	// A new client pointed at a dead server starts from the cache.
	offline, err := New(Config{BaseURL: "http://127.0.0.1:1", SDKKey: s.sdkKey, CacheFile: cache, DisableEvents: true})
	if err != nil {
		t.Fatal(err)
	}
	defer offline.Close()
	if err := offline.WaitReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !offline.Bool("checkout", eval.NewContext("u"), false) {
		t.Error("cached configuration not used")
	}

	if err := os.WriteFile(cache, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	broken, err := New(Config{BaseURL: "http://127.0.0.1:1", SDKKey: "k", CacheFile: cache, DisableEvents: true})
	if err != nil {
		t.Fatal(err)
	}
	defer broken.Close()
	if broken.Ready() {
		t.Error("corrupt cache should be ignored")
	}
}

func TestStaleSnapshotIgnored(t *testing.T) {
	c := &Client{cfg: Config{Logger: nopLogger()}, ready: make(chan struct{})}
	c.setSnapshot(model.NewSnapshot("prod", 5), false)
	c.setSnapshot(model.NewSnapshot("prod", 3), false)
	if c.Snapshot().Revision != 5 {
		t.Errorf("revision rolled back to %d", c.Snapshot().Revision)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	c, err := New(Config{BaseURL: "http://127.0.0.1:1", SDKKey: "k", DisableEvents: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkClientBool(b *testing.B) {
	c := &Client{cfg: Config{Logger: nopLogger(), DisableEvents: true}, ready: make(chan struct{})}
	snap := model.NewSnapshot("prod", 1)
	f := boolFlag("checkout")
	cfg := model.DefaultConfig(&f, "prod")
	cfg.Enabled = true
	snap.Flags["checkout"] = &model.FlagState{Flag: f, Config: cfg}
	c.setSnapshot(snap, false)
	ctx := eval.NewContext("user-1")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Bool("checkout", ctx, false)
	}
}

func nopLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

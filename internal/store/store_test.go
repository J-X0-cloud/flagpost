package store

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/J-X0-cloud/flagpost/internal/audit"
	"github.com/J-X0-cloud/flagpost/model"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Second)
	return c.t
}

func openTest(t *testing.T) *Store {
	t.Helper()
	clock := &fakeClock{t: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"), Options{Now: clock.Now})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func testFlag(key string, tags ...string) model.Flag {
	return model.Flag{
		Key: key, Name: strings.ToUpper(key), Kind: model.KindBoolean, Tags: tags,
		Variants: []model.Variant{
			{Key: "on", Value: json.RawMessage(`true`)},
			{Key: "off", Value: json.RawMessage(`false`)},
		},
	}
}

func mustEnv(t *testing.T, s *Store, key string) model.Environment {
	t.Helper()
	e, err := s.CreateEnvironment(context.Background(), model.Environment{Key: key, Name: key, SDKKey: "sdk-" + key})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestEnvironments(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	prod := mustEnv(t, s, "production")
	mustEnv(t, s, "staging")
	if prod.Revision != 1 || prod.CreatedAt.IsZero() {
		t.Errorf("unexpected env %+v", prod)
	}
	if _, err := s.CreateEnvironment(ctx, model.Environment{Key: "production", Name: "x", SDKKey: "other"}); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("duplicate create: %v", err)
	}
	got, err := s.GetEnvironmentBySDKKey(ctx, "sdk-staging")
	if err != nil || got.Key != "staging" {
		t.Fatalf("by sdk key: %+v %v", got, err)
	}
	if _, err := s.GetEnvironmentBySDKKey(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown sdk key: %v", err)
	}
	if err := s.SetSDKKey(ctx, "staging", "rotated"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetEnvironmentBySDKKey(ctx, "sdk-staging"); !errors.Is(err, ErrNotFound) {
		t.Error("old sdk key still resolves")
	}
	upd, err := s.UpdateEnvironment(ctx, "staging", "Staging", "pre-prod")
	if err != nil || upd.Name != "Staging" || upd.Description != "pre-prod" {
		t.Fatalf("update: %+v %v", upd, err)
	}
	rev, err := s.BumpRevision(ctx, "staging")
	if err != nil || rev != 2 {
		t.Fatalf("bump: %d %v", rev, err)
	}
	all, err := s.BumpAllRevisions(ctx)
	if err != nil || all["staging"] != 3 || all["production"] != 2 {
		t.Fatalf("bump all: %v %v", all, err)
	}
	list, _ := s.ListEnvironments(ctx)
	if len(list) != 2 || list[0].Key != "production" {
		t.Errorf("list order: %+v", list)
	}
	if err := s.DeleteEnvironment(ctx, "staging"); err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{
		s.DeleteEnvironment(ctx, "staging"),
		s.SetSDKKey(ctx, "staging", "x"),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("expected not found, got %v", err)
		}
	}
	if _, err := s.BumpRevision(ctx, "staging"); !errors.Is(err, ErrNotFound) {
		t.Errorf("bump missing env: %v", err)
	}
}

func TestFlagsCRUDAndConcurrency(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	f, err := s.CreateFlag(ctx, testFlag("checkout", "payments", "web"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Version != 1 {
		t.Errorf("version = %d", f.Version)
	}
	if _, err := s.CreateFlag(ctx, testFlag("checkout")); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("duplicate: %v", err)
	}
	got, err := s.GetFlag(ctx, "checkout")
	if err != nil || len(got.Variants) != 2 || got.Tags[1] != "web" || !got.CreatedAt.Equal(f.CreatedAt) {
		t.Fatalf("get: %+v %v", got, err)
	}

	got.Description = "faster checkout"
	upd, err := s.UpdateFlag(ctx, got, 1)
	if err != nil || upd.Version != 2 || upd.Description != "faster checkout" || !upd.UpdatedAt.After(f.UpdatedAt) {
		t.Fatalf("update: %+v %v", upd, err)
	}
	if _, err := s.UpdateFlag(ctx, got, 1); !errors.Is(err, ErrConflict) {
		t.Errorf("stale update: %v", err)
	}
	if _, err := s.UpdateFlag(ctx, testFlag("ghost"), 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("update missing: %v", err)
	}
	if err := s.DeleteFlag(ctx, "checkout"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFlag(ctx, "checkout"); !errors.Is(err, ErrNotFound) {
		t.Errorf("double delete: %v", err)
	}
}

func TestListFlagsFilters(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	for _, f := range []model.Flag{testFlag("alpha", "web"), testFlag("beta", "mobile"), testFlag("gamma_100", "web", "mobile")} {
		if _, err := s.CreateFlag(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	archived := testFlag("old")
	archived.Archived = true
	if _, err := s.CreateFlag(ctx, archived); err != nil {
		t.Fatal(err)
	}
	keys := func(fl FlagFilter) string {
		fs, err := s.ListFlags(ctx, fl)
		if err != nil {
			t.Fatal(err)
		}
		var ks []string
		for _, f := range fs {
			ks = append(ks, f.Key)
		}
		return strings.Join(ks, ",")
	}
	tests := []struct {
		filter FlagFilter
		want   string
	}{
		{FlagFilter{}, "alpha,beta,gamma_100"},
		{FlagFilter{IncludeArchived: true}, "alpha,beta,gamma_100,old"},
		{FlagFilter{Tag: "web"}, "alpha,gamma_100"},
		{FlagFilter{Tag: "mobile", Query: "GAM"}, "gamma_100"},
		{FlagFilter{Query: "a_1"}, "gamma_100"},
		{FlagFilter{Query: "a%"}, ""},
		{FlagFilter{Query: "ET"}, "beta"},
	}
	for _, tt := range tests {
		if got := keys(tt.filter); got != tt.want {
			t.Errorf("ListFlags(%+v) = %q, want %q", tt.filter, got, tt.want)
		}
	}
}

func TestFlagConfigs(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	mustEnv(t, s, "prod")
	f, _ := s.CreateFlag(ctx, testFlag("checkout"))
	if _, err := s.GetFlagConfig(ctx, "checkout", "prod"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected not found, got %v", err)
	}
	cfg := model.DefaultConfig(&f, "prod")
	cfg.Enabled = true
	cfg.Rules = []model.Rule{{ID: "r1", Clauses: []model.Clause{{Attribute: "country", Operator: model.OpEquals, Values: []any{"NZ"}}}, Serve: model.Serve{Variant: "on"}}}
	stored, err := s.PutFlagConfig(ctx, cfg, 0)
	if err != nil || stored.Version != 1 || !stored.Enabled || len(stored.Rules) != 1 {
		t.Fatalf("put: %+v %v", stored, err)
	}
	if _, err := s.PutFlagConfig(ctx, cfg, 0); !errors.Is(err, ErrConflict) {
		t.Errorf("second create: %v", err)
	}
	stored.Enabled = false
	v2, err := s.PutFlagConfig(ctx, stored, 1)
	if err != nil || v2.Version != 2 || v2.Enabled {
		t.Fatalf("update: %+v %v", v2, err)
	}
	if _, err := s.PutFlagConfig(ctx, stored, 1); !errors.Is(err, ErrConflict) {
		t.Errorf("stale update: %v", err)
	}
	if _, err := s.PutFlagConfig(ctx, model.FlagConfig{FlagKey: "ghost", Environment: "prod"}, 3); !errors.Is(err, ErrNotFound) {
		t.Errorf("update missing: %v", err)
	}
	all, err := s.ListFlagConfigs(ctx, "prod")
	if err != nil || len(all) != 1 || all["checkout"].Version != 2 {
		t.Fatalf("list: %v %v", all, err)
	}
}

func TestForeignKeysCascade(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	mustEnv(t, s, "prod")
	f, _ := s.CreateFlag(ctx, testFlag("checkout"))
	if _, err := s.PutFlagConfig(ctx, model.DefaultConfig(&f, "prod"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSegment(ctx, model.Segment{Environment: "prod", Key: "beta", Name: "Beta"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutFlagConfig(ctx, model.DefaultConfig(&f, "nowhere"), 0); err == nil {
		t.Error("config for a missing environment should violate the foreign key")
	}
	if err := s.DeleteFlag(ctx, "checkout"); err != nil {
		t.Fatal(err)
	}
	if cfgs, _ := s.ListFlagConfigs(ctx, "prod"); len(cfgs) != 0 {
		t.Error("deleting a flag left its configs behind")
	}
	if err := s.DeleteEnvironment(ctx, "prod"); err != nil {
		t.Fatal(err)
	}
	if segs, _ := s.ListSegments(ctx, "prod"); len(segs) != 0 {
		t.Error("deleting an environment left its segments behind")
	}
}

func TestFlagsDependingOn(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	mustEnv(t, s, "prod")
	mustEnv(t, s, "dev")
	for _, k := range []string{"parent", "child-a", "child-b", "loner"} {
		if _, err := s.CreateFlag(ctx, testFlag(k)); err != nil {
			t.Fatal(err)
		}
	}
	put := func(flag, env string, prereqs ...model.Prerequisite) {
		f, _ := s.GetFlag(ctx, flag)
		c := model.DefaultConfig(&f, env)
		c.Prerequisites = prereqs
		if _, err := s.PutFlagConfig(ctx, c, 0); err != nil {
			t.Fatal(err)
		}
	}
	put("child-a", "prod", model.Prerequisite{Flag: "parent", Variant: "on"})
	put("child-b", "dev", model.Prerequisite{Flag: "other", Variant: "on"}, model.Prerequisite{Flag: "parent", Variant: "off"})
	put("loner", "prod")
	deps, err := s.FlagsDependingOn(ctx, "parent")
	if err != nil || strings.Join(deps, ",") != "child-a,child-b" {
		t.Fatalf("deps = %v, %v", deps, err)
	}
}

func TestSegments(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	mustEnv(t, s, "prod")
	w := 5000
	seg := model.Segment{Environment: "prod", Key: "beta", Name: "Beta", Included: []string{"u1"},
		Rules: []model.SegmentRule{{ID: "r", Weight: &w, Clauses: []model.Clause{{Attribute: "plan", Operator: model.OpEquals, Values: []any{"pro"}}}}}}
	created, err := s.CreateSegment(ctx, seg)
	if err != nil || created.Version != 1 || *created.Rules[0].Weight != 5000 {
		t.Fatalf("create: %+v %v", created, err)
	}
	if _, err := s.CreateSegment(ctx, seg); !errors.Is(err, ErrAlreadyExists) {
		t.Errorf("duplicate: %v", err)
	}
	created.Excluded = []string{"u2"}
	upd, err := s.UpdateSegment(ctx, created, 1)
	if err != nil || upd.Version != 2 || upd.Excluded[0] != "u2" {
		t.Fatalf("update: %+v %v", upd, err)
	}
	if _, err := s.UpdateSegment(ctx, created, 1); !errors.Is(err, ErrConflict) {
		t.Errorf("stale: %v", err)
	}
	if err := s.DeleteSegment(ctx, "prod", "beta"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSegment(ctx, "prod", "beta"); !errors.Is(err, ErrNotFound) {
		t.Errorf("get deleted: %v", err)
	}
	if err := s.DeleteSegment(ctx, "prod", "beta"); !errors.Is(err, ErrNotFound) {
		t.Errorf("double delete: %v", err)
	}
}

func TestAuditLog(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	for i := 0; i < 7; i++ {
		env := "prod"
		if i%2 == 1 {
			env = "dev"
		}
		_, err := s.InsertAudit(ctx, audit.Entry{
			Actor: "admin", Action: audit.ActionUpdate, ResourceType: audit.ResourceFlagConfig,
			ResourceKey: "checkout", Environment: env, After: json.RawMessage(`{"enabled":true}`),
			Changes: []audit.Change{{Path: "enabled", Op: audit.OpReplace, Old: json.RawMessage(`false`), New: json.RawMessage(`true`)}},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	page1, err := s.ListAudit(ctx, audit.Filter{Environment: "prod", Limit: 2})
	if err != nil || len(page1) != 2 || page1[0].ID <= page1[1].ID {
		t.Fatalf("page1: %+v %v", page1, err)
	}
	page2, _ := s.ListAudit(ctx, audit.Filter{Environment: "prod", Limit: 2, BeforeID: page1[1].ID})
	if len(page2) != 2 || page2[0].ID >= page1[1].ID {
		t.Fatalf("page2: %+v", page2)
	}
	all, _ := s.ListAudit(ctx, audit.Filter{ResourceKey: "checkout"})
	if len(all) != 7 {
		t.Errorf("all = %d", len(all))
	}
	e := all[0]
	if e.Before != nil || string(e.After) != `{"enabled":true}` || len(e.Changes) != 1 || e.At.IsZero() {
		t.Errorf("round trip lost data: %+v", e)
	}
	none, _ := s.ListAudit(ctx, audit.Filter{Actor: "someone-else"})
	if len(none) != 0 {
		t.Errorf("actor filter returned %d", len(none))
	}
}

func TestExperimentEvents(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	mustEnv(t, s, "prod")
	t0 := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	at := func(m int) time.Time { return t0.Add(time.Duration(m) * time.Minute) }

	err := s.RecordExposures(ctx, "prod", []Exposure{
		{Flag: "exp", ContextKey: "u1", Variant: "control", At: at(10)},
		{Flag: "exp", ContextKey: "u2", Variant: "treatment", At: at(10)},
		{Flag: "exp", ContextKey: "u3", Variant: "treatment", At: at(10)},
		{Flag: "exp", ContextKey: "u4", Variant: "control", At: at(10)},
		// Duplicate later exposure for u1 with a different variant is ignored.
		{Flag: "exp", ContextKey: "u1", Variant: "treatment", At: at(20)},
		// An earlier exposure arriving late wins.
		{Flag: "exp", ContextKey: "u4", Variant: "treatment", At: at(5)},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = s.RecordConversions(ctx, "prod", []Conversion{
		{Event: "purchase", ContextKey: "u1", At: at(11)},
		{Event: "purchase", ContextKey: "u2", At: at(9)},  // before exposure: not attributed
		{Event: "purchase", ContextKey: "u3", At: at(30)}, // attributed
		{Event: "purchase", ContextKey: "u3", At: at(31)}, // duplicate
		{Event: "signup", ContextKey: "u4", At: at(30)},   // other event
	})
	if err != nil {
		t.Fatal(err)
	}
	counts, err := s.VariantCounts(ctx, "prod", "exp", "purchase")
	if err != nil {
		t.Fatal(err)
	}
	want := []VariantCount{{"control", 1, 1}, {"treatment", 3, 1}}
	if len(counts) != len(want) {
		t.Fatalf("counts = %+v", counts)
	}
	for i := range want {
		if counts[i] != want[i] {
			t.Errorf("counts[%d] = %+v, want %+v", i, counts[i], want[i])
		}
	}
	n, err := s.ResetExperiment(ctx, "prod", "exp")
	if err != nil || n != 4 {
		t.Fatalf("reset removed %d, %v", n, err)
	}
}

func TestSnapshot(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	env := mustEnv(t, s, "prod")
	configured, _ := s.CreateFlag(ctx, testFlag("configured"))
	if _, err := s.CreateFlag(ctx, testFlag("unconfigured")); err != nil {
		t.Fatal(err)
	}
	arch := testFlag("archived")
	arch.Archived = true
	if _, err := s.CreateFlag(ctx, arch); err != nil {
		t.Fatal(err)
	}
	cfg := model.DefaultConfig(&configured, "prod")
	cfg.Enabled = true
	if _, err := s.PutFlagConfig(ctx, cfg, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateSegment(ctx, model.Segment{Environment: "prod", Key: "beta", Name: "Beta"}); err != nil {
		t.Fatal(err)
	}
	snap, err := s.Snapshot(ctx, "prod")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Revision != env.Revision || len(snap.Flags) != 2 || len(snap.Segments) != 1 {
		t.Fatalf("snapshot: rev %d flags %d segs %d", snap.Revision, len(snap.Flags), len(snap.Segments))
	}
	if !snap.Flags["configured"].Config.Enabled || snap.Flags["configured"].Config.Version != 1 {
		t.Error("stored config not used")
	}
	if u := snap.Flags["unconfigured"].Config; u.Enabled || u.OffVariant != "off" || u.Version != 0 {
		t.Errorf("default config not applied: %+v", u)
	}
	if _, err := s.Snapshot(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing env: %v", err)
	}
}

func TestTxRollback(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	boom := errors.New("boom")
	err := s.Tx(ctx, func(tx *Tx) error {
		if _, err := tx.CreateFlag(ctx, testFlag("temp")); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if _, err := s.GetFlag(ctx, "temp"); !errors.Is(err, ErrNotFound) {
		t.Error("rolled back insert is visible")
	}
}

func TestConcurrentWriters(t *testing.T) {
	ctx := context.Background()
	s := openTest(t)
	mustEnv(t, s, "prod")
	var wg sync.WaitGroup
	errs := make(chan error, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- s.Tx(ctx, func(tx *Tx) error {
				_, err := tx.BumpRevision(ctx, "prod")
				return err
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	e, _ := s.GetEnvironment(ctx, "prod")
	if e.Revision != 51 {
		t.Errorf("revision = %d, want 51 (lost updates)", e.Revision)
	}
}

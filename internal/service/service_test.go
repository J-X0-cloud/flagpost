package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/J-X0-cloud/flagpost/eval"
	"github.com/J-X0-cloud/flagpost/internal/audit"
	"github.com/J-X0-cloud/flagpost/internal/store"
	"github.com/J-X0-cloud/flagpost/internal/stream"
	"github.com/J-X0-cloud/flagpost/model"
)

type fixture struct {
	svc *Service
	ctx context.Context
	t   *testing.T
}

func newFixture(t *testing.T, envs ...string) *fixture {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "svc.db"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &fixture{svc: New(st, stream.NewBroker(stream.Options{}), nil), ctx: WithActor(context.Background(), "tester"), t: t}
	for _, e := range envs {
		if _, err := f.svc.CreateEnvironment(f.ctx, model.Environment{Key: e, Name: e}); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func boolFlag(key string) model.Flag {
	return model.Flag{Key: key, Name: key, Kind: model.KindBoolean, Variants: []model.Variant{
		{Key: "on", Value: json.RawMessage(`true`)},
		{Key: "off", Value: json.RawMessage(`false`)},
	}}
}

func (f *fixture) flag(key string) model.Flag {
	f.t.Helper()
	fl, err := f.svc.CreateFlag(f.ctx, boolFlag(key))
	if err != nil {
		f.t.Fatal(err)
	}
	return fl
}

func (f *fixture) subscribe(env string) *stream.Subscription {
	f.t.Helper()
	sub, _, _, err := f.svc.Broker().Subscribe(env, "")
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(sub.Close)
	return sub
}

func next(t *testing.T, sub *stream.Subscription) stream.Event {
	t.Helper()
	select {
	case ev := <-sub.C:
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("no stream event")
		return stream.Event{}
	}
}

func assertProblem(t *testing.T, err error, fragment string) {
	t.Helper()
	var ve *model.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("expected validation error containing %q, got %v", fragment, err)
	}
	if !strings.Contains(ve.Error(), fragment) {
		t.Fatalf("validation error %q does not mention %q", ve.Error(), fragment)
	}
}

func TestActor(t *testing.T) {
	if Actor(context.Background()) != "system" || Actor(WithActor(context.Background(), "ann")) != "ann" {
		t.Error("actor propagation broken")
	}
}

func TestEnvironmentLifecycle(t *testing.T) {
	f := newFixture(t, "prod")
	env, _ := f.svc.GetEnvironment(f.ctx, "prod")
	if !strings.HasPrefix(env.SDKKey, "fp-sdk-") || len(env.SDKKey) != 7+48 {
		t.Errorf("unexpected sdk key %q", env.SDKKey)
	}
	if _, err := f.svc.CreateEnvironment(f.ctx, model.Environment{Key: "bad key", Name: "x"}); err == nil {
		t.Error("invalid key accepted")
	}
	rotated, err := f.svc.RotateSDKKey(f.ctx, "prod")
	if err != nil || rotated.SDKKey == env.SDKKey {
		t.Fatalf("rotate: %v", err)
	}
	if _, err := f.svc.EnvironmentForSDKKey(f.ctx, env.SDKKey); !errors.Is(err, store.ErrNotFound) {
		t.Error("old key still authenticates")
	}
	if _, err := f.svc.EnvironmentForSDKKey(f.ctx, ""); !errors.Is(err, store.ErrNotFound) {
		t.Error("empty key authenticates")
	}
	if got, err := f.svc.EnvironmentForSDKKey(f.ctx, rotated.SDKKey); err != nil || got.Key != "prod" {
		t.Errorf("new key: %v", err)
	}
	if _, err := f.svc.UpdateEnvironment(f.ctx, "prod", "Production", "live"); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.DeleteEnvironment(f.ctx, "prod"); err != nil {
		t.Fatal(err)
	}
	entries, _ := f.svc.ListAudit(f.ctx, audit.Filter{ResourceType: audit.ResourceEnvironment})
	var actions []string
	for _, e := range entries {
		actions = append(actions, e.Action)
		if strings.Contains(string(e.Before)+string(e.After), "fp-sdk-") {
			t.Errorf("SDK key leaked into audit entry %d", e.ID)
		}
		if e.Actor != "tester" {
			t.Errorf("actor = %q", e.Actor)
		}
	}
	if strings.Join(actions, ",") != "delete,update,rotate_key,create" {
		t.Errorf("actions = %v", actions)
	}
}

func TestCreateFlagPublishesToEveryEnvironment(t *testing.T) {
	f := newFixture(t, "prod", "dev")
	prod, dev := f.subscribe("prod"), f.subscribe("dev")
	f.flag("checkout")
	for _, sub := range []*stream.Subscription{prod, dev} {
		ev := next(t, sub)
		var p model.StreamPatch
		if err := json.Unmarshal(ev.Data, &p); err != nil || ev.Type != model.EventPatch {
			t.Fatalf("event %s: %v", ev.Type, err)
		}
		if p.Flag == nil || p.Flag.Flag.Key != "checkout" || p.Flag.Config.Enabled || p.Revision != 2 {
			t.Errorf("unexpected patch %+v", p)
		}
	}
	if _, err := f.svc.CreateFlag(f.ctx, boolFlag("checkout")); !errors.Is(err, store.ErrAlreadyExists) {
		t.Errorf("duplicate: %v", err)
	}
}

func TestFlagConfigUpdateAndToggle(t *testing.T) {
	f := newFixture(t, "prod")
	f.flag("checkout")
	sub := f.subscribe("prod")

	cfg, err := f.svc.GetFlagConfig(f.ctx, "prod", "checkout")
	if err != nil || cfg.Version != 0 || cfg.Enabled {
		t.Fatalf("default config: %+v %v", cfg, err)
	}
	cfg.Rules = []model.Rule{{Clauses: []model.Clause{{Attribute: "country", Operator: model.OpEquals, Values: []any{"NZ"}}}, Serve: model.Serve{Variant: "on"}}}
	cfg.Fallthrough = model.Serve{Variant: "off"}
	saved, err := f.svc.UpdateFlagConfig(f.ctx, "prod", "checkout", cfg, 0, "target NZ")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Version != 1 || saved.Rules[0].ID == "" || saved.Salt != "checkout" {
		t.Errorf("saved: %+v", saved)
	}
	if ev := next(t, sub); ev.Type != model.EventPatch {
		t.Errorf("event type %s", ev.Type)
	}
	if _, err := f.svc.UpdateFlagConfig(f.ctx, "prod", "checkout", cfg, 0, ""); !errors.Is(err, store.ErrConflict) {
		t.Errorf("stale write: %v", err)
	}

	toggled, err := f.svc.ToggleFlag(f.ctx, "prod", "checkout", true, "launch")
	if err != nil || !toggled.Enabled || toggled.Version != 2 || len(toggled.Rules) != 1 {
		t.Fatalf("toggle: %+v %v", toggled, err)
	}

	entries, _ := f.svc.ListAudit(f.ctx, audit.Filter{ResourceType: audit.ResourceFlagConfig})
	if len(entries) != 2 || entries[0].Action != audit.ActionToggle || entries[0].Comment != "launch" {
		t.Fatalf("audit: %+v", entries)
	}
	if len(entries[0].Changes) != 1 || entries[0].Changes[0].Path != "enabled" {
		t.Errorf("toggle diff: %+v", entries[0].Changes)
	}

	// The evaluator sees the change through the snapshot cache.
	res, err := f.svc.Evaluate(f.ctx, "prod", "checkout", eval.NewContext("u").With("country", "NZ"), true)
	if err != nil || res.Variant != "on" || res.Reason != eval.ReasonRuleMatch || len(res.Trace) == 0 {
		t.Errorf("evaluate: %+v %v", res, err)
	}
	all, _ := f.svc.EvaluateAll(f.ctx, "prod", eval.NewContext("u"))
	if len(all) != 1 || all[0].Variant != "off" {
		t.Errorf("evaluate all: %+v", all)
	}
}

func TestNoOpSaveIsNotAudited(t *testing.T) {
	f := newFixture(t, "prod")
	f.flag("checkout")
	cfg, _ := f.svc.GetFlagConfig(f.ctx, "prod", "checkout")
	saved, err := f.svc.UpdateFlagConfig(f.ctx, "prod", "checkout", cfg, AnyVersion, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.UpdateFlagConfig(f.ctx, "prod", "checkout", saved, AnyVersion, ""); err != nil {
		t.Fatal(err)
	}
	entries, _ := f.svc.ListAudit(f.ctx, audit.Filter{ResourceType: audit.ResourceFlagConfig})
	if len(entries) != 0 {
		t.Errorf("no-op saves produced %d audit entries", len(entries))
	}
}

func TestConfigReferenceChecks(t *testing.T) {
	f := newFixture(t, "prod")
	for _, k := range []string{"a", "b", "c"} {
		f.flag(k)
	}
	put := func(flag string, mutate func(*model.FlagConfig)) error {
		cfg, _ := f.svc.GetFlagConfig(f.ctx, "prod", flag)
		mutate(&cfg)
		_, err := f.svc.UpdateFlagConfig(f.ctx, "prod", flag, cfg, AnyVersion, "")
		return err
	}
	prereq := func(flag, variant string) func(*model.FlagConfig) {
		return func(c *model.FlagConfig) {
			c.Prerequisites = []model.Prerequisite{{Flag: flag, Variant: variant}}
		}
	}
	if err := put("a", prereq("b", "on")); err != nil {
		t.Fatal(err)
	}
	if err := put("b", prereq("c", "on")); err != nil {
		t.Fatal(err)
	}
	assertProblem(t, put("c", prereq("a", "on")), "c -> a -> b -> c")
	assertProblem(t, put("c", prereq("ghost", "on")), `flag "ghost" does not exist`)
	assertProblem(t, put("c", prereq("a", "maybe")), `has no variant "maybe"`)
	assertProblem(t, put("c", func(c *model.FlagConfig) {
		c.Rules = []model.Rule{{ID: "r", Clauses: []model.Clause{{Operator: model.OpInSegment, Values: []any{"nobody"}}}, Serve: model.Serve{Variant: "on"}}}
	}), `segment "nobody" does not exist`)
	assertProblem(t, put("c", func(c *model.FlagConfig) { c.OffVariant = "nope" }), "unknown variant")

	if err := f.svc.DeleteFlag(f.ctx, "b"); !errors.Is(err, ErrInUse) || !strings.Contains(err.Error(), "prerequisite of a") {
		t.Errorf("delete prerequisite: %v", err)
	}
	if err := f.svc.DeleteFlag(f.ctx, "a"); err != nil {
		t.Errorf("delete leaf: %v", err)
	}
	if _, err := f.svc.GetFlagConfig(f.ctx, "nowhere", "b"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing env: %v", err)
	}
}

func TestFindCycle(t *testing.T) {
	tests := []struct {
		graph map[string][]string
		start string
		want  string
	}{
		{map[string][]string{"a": {"b"}, "b": {"c"}}, "a", ""},
		{map[string][]string{"a": {"a"}}, "a", "a,a"},
		{map[string][]string{"a": {"b"}, "b": {"c"}, "c": {"b"}}, "a", "b,c,b"},
		{map[string][]string{"a": {"b", "c"}, "b": {"d"}, "c": {"d"}, "d": nil}, "a", ""},
	}
	for _, tt := range tests {
		if got := strings.Join(findCycle(tt.graph, tt.start), ","); got != tt.want {
			t.Errorf("findCycle(%v) = %q, want %q", tt.graph, got, tt.want)
		}
	}
}

func TestUpdateFlagGuards(t *testing.T) {
	f := newFixture(t, "prod")
	fl := f.flag("checkout")
	cfg, _ := f.svc.GetFlagConfig(f.ctx, "prod", "checkout")
	if _, err := f.svc.UpdateFlagConfig(f.ctx, "prod", "checkout", cfg, 0, ""); err != nil {
		t.Fatal(err)
	}

	changed := fl
	changed.Kind = model.KindString
	changed.Variants = []model.Variant{{Key: "on", Value: json.RawMessage(`"x"`)}}
	if _, err := f.svc.UpdateFlag(f.ctx, changed, fl.Version); !errors.Is(err, ErrInvalid) {
		t.Errorf("kind change: %v", err)
	}

	renamed := fl
	renamed.Variants = []model.Variant{
		{Key: "enabled", Value: json.RawMessage(`true`)},
		{Key: "off", Value: json.RawMessage(`false`)},
	}
	_, err := f.svc.UpdateFlag(f.ctx, renamed, fl.Version)
	assertProblem(t, err, `variant "on" is still served in environment "prod"`)

	desc := fl
	desc.Description = "now with docs"
	upd, err := f.svc.UpdateFlag(f.ctx, desc, fl.Version)
	if err != nil || upd.Version != 2 {
		t.Fatalf("update: %+v %v", upd, err)
	}
	if _, err := f.svc.UpdateFlag(f.ctx, desc, fl.Version); !errors.Is(err, store.ErrConflict) {
		t.Errorf("stale: %v", err)
	}

	sub := f.subscribe("prod")
	archived := upd
	archived.Archived = true
	if _, err := f.svc.UpdateFlag(f.ctx, archived, upd.Version); err != nil {
		t.Fatal(err)
	}
	if ev := next(t, sub); ev.Type != model.EventDelete {
		t.Errorf("archiving should publish a delete, got %s", ev.Type)
	}
	if _, err := f.svc.ToggleFlag(f.ctx, "prod", "checkout", true, ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("toggling an archived flag: %v", err)
	}
	snap, _ := f.svc.Snapshot(f.ctx, "prod")
	if _, ok := snap.Flags["checkout"]; ok {
		t.Error("archived flag is in the snapshot")
	}
}

func TestSegments(t *testing.T) {
	f := newFixture(t, "prod")
	f.flag("checkout")
	mk := func(key string, refs ...string) model.Segment {
		s := model.Segment{Environment: "prod", Key: key, Name: key, Included: []string{"u2", "u1", "u2"}}
		for _, r := range refs {
			s.Rules = append(s.Rules, model.SegmentRule{Clauses: []model.Clause{{Operator: model.OpInSegment, Values: []any{r}}}})
		}
		return s
	}
	beta, err := f.svc.CreateSegment(f.ctx, mk("beta"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(beta.Included, ",") != "u1,u2" {
		t.Errorf("included not normalised: %v", beta.Included)
	}
	if _, err := f.svc.CreateSegment(f.ctx, mk("staff", "beta")); err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.CreateSegment(f.ctx, mk("ghostly", "ghost"))
	assertProblem(t, err, `segment "ghost" does not exist`)
	_, err = f.svc.UpdateSegment(f.ctx, mk("beta", "staff"), AnyVersion)
	assertProblem(t, err, "beta -> staff -> beta")

	cfg, _ := f.svc.GetFlagConfig(f.ctx, "prod", "checkout")
	cfg.Rules = []model.Rule{{ID: "r", Clauses: []model.Clause{{Operator: model.OpInSegment, Values: []any{"beta"}}}, Serve: model.Serve{Variant: "on"}}}
	cfg.Enabled = true
	cfg.Fallthrough = model.Serve{Variant: "off"}
	if _, err := f.svc.UpdateFlagConfig(f.ctx, "prod", "checkout", cfg, 0, ""); err != nil {
		t.Fatal(err)
	}
	err = f.svc.DeleteSegment(f.ctx, "prod", "beta")
	if !errors.Is(err, ErrInUse) || !strings.Contains(err.Error(), "flag checkout, segment staff") {
		t.Errorf("delete in use: %v", err)
	}

	res, _ := f.svc.Evaluate(f.ctx, "prod", "checkout", eval.NewContext("u1"), false)
	if res.Variant != "on" {
		t.Errorf("segment member got %+v", res)
	}
	upd := beta
	upd.Included = nil
	if _, err := f.svc.UpdateSegment(f.ctx, upd, beta.Version); err != nil {
		t.Fatal(err)
	}
	res, _ = f.svc.Evaluate(f.ctx, "prod", "checkout", eval.NewContext("u1"), false)
	if res.Variant != "off" {
		t.Errorf("snapshot cache not invalidated by segment update: %+v", res)
	}

	if err := f.svc.DeleteSegment(f.ctx, "prod", "staff"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.ListSegments(f.ctx, "nowhere"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("list in missing env: %v", err)
	}
}

func TestExperimentFlow(t *testing.T) {
	f := newFixture(t, "prod")
	_, err := f.svc.CreateFlag(f.ctx, model.Flag{Key: "cta", Name: "CTA", Kind: model.KindString, Variants: []model.Variant{
		{Key: "control", Value: json.RawMessage(`"Buy"`)},
		{Key: "treatment", Value: json.RawMessage(`"Buy now"`)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := f.svc.GetFlagConfig(f.ctx, "prod", "cta")
	cfg.Enabled = true
	cfg.Fallthrough = model.Serve{Rollout: &model.Rollout{Experiment: true, Variants: []model.WeightedVariant{
		{Variant: "control", Weight: 50000}, {Variant: "treatment", Weight: 50000},
	}}}
	if _, err := f.svc.UpdateFlagConfig(f.ctx, "prod", "cta", cfg, 0, ""); err != nil {
		t.Fatal(err)
	}

	ev, _ := f.svc.Evaluator(f.ctx, "prod")
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	var batch model.EventBatch
	for i := 0; i < 3000; i++ {
		key := fmt.Sprintf("user-%d", i)
		r := ev.Evaluate("cta", eval.NewContext(key))
		batch.Exposures = append(batch.Exposures, model.ExposureEvent{Flag: "cta", Variant: r.Variant, ContextKey: key, Timestamp: now})
		// Treatment converts at 30%, control at 20%, deterministically.
		threshold := 20
		if r.Variant == "treatment" {
			threshold = 30
		}
		if i%100 < threshold {
			batch.Conversions = append(batch.Conversions, model.ConversionEvent{Event: "purchase", ContextKey: key, Timestamp: now.Add(time.Minute)})
		}
	}
	batch.Exposures = append(batch.Exposures,
		model.ExposureEvent{Flag: "ghost", Variant: "x", ContextKey: "u"},
		model.ExposureEvent{Flag: "cta", Variant: "nope", ContextKey: "u"},
		model.ExposureEvent{Flag: "cta", Variant: "control"})
	batch.Conversions = append(batch.Conversions, model.ConversionEvent{Event: "bad event!", ContextKey: "u"})

	res, err := f.svc.RecordEvents(f.ctx, "prod", batch, now)
	if err != nil {
		t.Fatal(err)
	}
	if res.Rejected != 4 || res.Accepted != len(batch.Exposures)+len(batch.Conversions)-4 {
		t.Errorf("record: %+v", res)
	}

	out, err := f.svc.ExperimentResults(f.ctx, "prod", "cta", "purchase", "", 0.95)
	if err != nil {
		t.Fatal(err)
	}
	if out.Control != "control" || len(out.Arms) != 2 {
		t.Fatalf("results: %+v", out)
	}
	tr := out.Arms[1]
	if tr.Variant != "treatment" || !tr.Significant || tr.Lift == nil || *tr.Lift < 0.3 {
		t.Errorf("treatment arm: %+v", tr)
	}
	if out.SRMPValue == nil || out.SRMDetected {
		t.Errorf("SRM: %+v", out)
	}

	for _, bad := range []struct {
		metric, control string
		conf            float64
	}{{"bad metric", "", 0.95}, {"purchase", "", 1.5}, {"purchase", "ghost", 0.95}} {
		if _, err := f.svc.ExperimentResults(f.ctx, "prod", "cta", bad.metric, bad.control, bad.conf); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	big := model.EventBatch{Conversions: make([]model.ConversionEvent, model.MaxEventBatch+1)}
	if _, err := f.svc.RecordEvents(f.ctx, "prod", big, now); !errors.Is(err, ErrInvalid) {
		t.Errorf("oversized batch: %v", err)
	}
}

func TestSnapshotCache(t *testing.T) {
	f := newFixture(t, "prod")
	f.flag("a")
	s1, _ := f.svc.Snapshot(f.ctx, "prod")
	s2, _ := f.svc.Snapshot(f.ctx, "prod")
	if s1 != s2 {
		t.Error("unchanged environment should hit the cache")
	}
	f.flag("b")
	s3, _ := f.svc.Snapshot(f.ctx, "prod")
	if s3 == s1 || len(s3.Flags) != 2 || s3.Revision <= s1.Revision {
		t.Errorf("cache not refreshed: rev %d -> %d", s1.Revision, s3.Revision)
	}
}

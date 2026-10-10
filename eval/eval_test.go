package eval

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/J-X0-cloud/flagpost/model"
)

// boolState returns an enabled boolean flag that falls through to "on".
func boolState(key string) *model.FlagState {
	return &model.FlagState{
		Flag: model.Flag{Key: key, Name: key, Kind: model.KindBoolean, Variants: []model.Variant{
			{Key: "on", Value: json.RawMessage(`true`)},
			{Key: "off", Value: json.RawMessage(`false`)},
		}},
		Config: model.FlagConfig{
			FlagKey: key, Enabled: true, OffVariant: "off",
			Fallthrough: model.Serve{Variant: "on"}, Salt: key, Version: 3,
		},
	}
}

func colorState(key string) *model.FlagState {
	return &model.FlagState{
		Flag: model.Flag{Key: key, Name: key, Kind: model.KindString, Variants: []model.Variant{
			{Key: "red", Value: json.RawMessage(`"red"`)},
			{Key: "green", Value: json.RawMessage(`"green"`)},
			{Key: "blue", Value: json.RawMessage(`"blue"`)},
		}},
		Config: model.FlagConfig{
			FlagKey: key, Enabled: true, OffVariant: "red",
			Fallthrough: model.Serve{Variant: "red"}, Salt: key,
		},
	}
}

func snapshot(flags []*model.FlagState, segs ...*model.Segment) *model.Snapshot {
	s := model.NewSnapshot("test", 1)
	for _, f := range flags {
		s.Flags[f.Flag.Key] = f
	}
	for _, sg := range segs {
		s.Segments[sg.Key] = sg
	}
	return s
}

func clause(attr string, op model.Operator, values ...any) model.Clause {
	return model.Clause{Attribute: attr, Operator: op, Values: values}
}

func rule(id, variant string, cs ...model.Clause) model.Rule {
	return model.Rule{ID: id, Clauses: cs, Serve: model.Serve{Variant: variant}}
}

func ptr(n int) *int { return &n }

func TestEvaluateBasics(t *testing.T) {
	ff := colorState("color")
	ff.Config.Targets = []model.Target{
		{Variant: "blue", Values: []string{"vip-1", "vip-2"}},
		{Variant: "green", Values: []string{"vip-2"}},
	}
	ff.Config.Rules = []model.Rule{
		rule("nz", "green", clause("country", model.OpEquals, "NZ")),
		rule("pro-nz", "blue", clause("country", model.OpEquals, "NZ"), clause("plan", model.OpEquals, "pro")),
		rule("beta", "blue", clause("email", model.OpEndsWith, "@beta.test")),
	}
	off := colorState("off-flag")
	off.Config.Enabled = false
	off.Config.OffVariant = "green"
	ev := New(snapshot([]*model.FlagState{ff, off}))

	tests := []struct {
		name    string
		flag    string
		ctx     Context
		variant string
		reason  Reason
		ruleID  string
	}{
		{"off", "off-flag", NewContext("u"), "green", ReasonOff, ""},
		{"target", "color", NewContext("vip-1"), "blue", ReasonTargetMatch, ""},
		{"first target wins", "color", NewContext("vip-2"), "blue", ReasonTargetMatch, ""},
		{"target beats rules", "color", NewContext("vip-1").With("country", "NZ"), "blue", ReasonTargetMatch, ""},
		{"first matching rule wins", "color", NewContext("u").With("country", "NZ").With("plan", "pro"), "green", ReasonRuleMatch, "nz"},
		{"later rule", "color", NewContext("u").With("email", "a@beta.test"), "blue", ReasonRuleMatch, "beta"},
		{"fallthrough", "color", NewContext("u").With("country", "AU"), "red", ReasonFallthrough, ""},
		{"empty context falls through", "color", Context{}, "red", ReasonFallthrough, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := ev.Evaluate(tt.flag, tt.ctx)
			if r.Variant != tt.variant || r.Reason != tt.reason || r.RuleID != tt.ruleID {
				t.Errorf("got variant=%q reason=%s rule=%q (%s), want %q %s %q",
					r.Variant, r.Reason, r.RuleID, r.Message, tt.variant, tt.reason, tt.ruleID)
			}
			if tt.reason == ReasonRuleMatch && r.RuleIndex < 0 {
				t.Error("rule match should carry a rule index")
			}
			if tt.reason != ReasonRuleMatch && r.RuleIndex != -1 {
				t.Errorf("RuleIndex = %d, want -1", r.RuleIndex)
			}
			var v string
			if err := json.Unmarshal(r.Value, &v); err != nil || v != tt.variant {
				t.Errorf("value %s does not match variant %q", r.Value, tt.variant)
			}
		})
	}
}

func TestEvaluateFlagNotFound(t *testing.T) {
	r := New(nil).Evaluate("missing", NewContext("u"))
	if !r.Failed() || r.Error != ErrFlagNotFound || r.Value != nil {
		t.Errorf("unexpected result %+v", r)
	}
}

func TestEvaluateUnknownVariantIsMalformed(t *testing.T) {
	f := boolState("f")
	f.Config.Fallthrough = model.Serve{Variant: "maybe"}
	r := New(snapshot([]*model.FlagState{f})).Evaluate("f", NewContext("u"))
	if r.Error != ErrMalformedFlag {
		t.Errorf("got %+v, want MALFORMED_FLAG", r)
	}
}

func TestClauseSemantics(t *testing.T) {
	tests := []struct {
		name  string
		cl    model.Clause
		attrs map[string]any
		match bool
	}{
		{"values are ORed", clause("country", model.OpIn, "NZ", "AU"), map[string]any{"country": "AU"}, true},
		{"missing attribute", clause("country", model.OpEquals, "NZ"), nil, false},
		{"missing attribute with negate", model.Clause{Attribute: "country", Operator: model.OpEquals, Values: []any{"NZ"}, Negate: true}, nil, false},
		{"negate", model.Clause{Attribute: "country", Operator: model.OpEquals, Values: []any{"NZ"}, Negate: true}, map[string]any{"country": "AU"}, true},
		{"null attribute", clause("country", model.OpEquals, "NZ"), map[string]any{"country": nil}, false},
		{"neq none of", clause("country", model.OpNotEquals, "NZ", "AU"), map[string]any{"country": "US"}, true},
		{"neq one of", clause("country", model.OpNotEquals, "NZ", "AU"), map[string]any{"country": "NZ"}, false},
		{"array attribute any element", clause("groups", model.OpEquals, "admins"), map[string]any{"groups": []string{"staff", "admins"}}, true},
		{"array attribute no element", clause("groups", model.OpEquals, "admins"), map[string]any{"groups": []string{"staff"}}, false},
		{"neq over array", clause("groups", model.OpNotEquals, "banned"), map[string]any{"groups": []any{"staff", "banned"}}, false},
		{"exists", clause("email", model.OpExists), map[string]any{"email": "a@b"}, true},
		{"exists null", clause("email", model.OpExists), map[string]any{"email": nil}, false},
		{"not exists", model.Clause{Attribute: "email", Operator: model.OpExists, Negate: true}, nil, true},
		{"nested path", clause("org.tier", model.OpEquals, "gold"), map[string]any{"org": map[string]any{"tier": "gold"}}, true},
		{"numeric int attribute", clause("seats", model.OpGreaterEq, float64(10)), map[string]any{"seats": 10}, true},
		{"semver app version", clause("appVersion", model.OpSemverLt, "3.0.0"), map[string]any{"appVersion": "v2.14.1"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := boolState("f")
			f.Config.Fallthrough = model.Serve{Variant: "off"}
			f.Config.Rules = []model.Rule{{ID: "r", Clauses: []model.Clause{tt.cl}, Serve: model.Serve{Variant: "on"}}}
			r := New(snapshot([]*model.FlagState{f})).Evaluate("f", Context{Key: "u", Attributes: tt.attrs})
			if got := r.Reason == ReasonRuleMatch; got != tt.match {
				t.Errorf("match = %v, want %v (%+v)", got, tt.match, r)
			}
		})
	}
}

func TestPrerequisites(t *testing.T) {
	parent := boolState("parent")
	child := boolState("child")
	child.Config.Prerequisites = []model.Prerequisite{{Flag: "parent", Variant: "on"}}

	t.Run("satisfied", func(t *testing.T) {
		r := New(snapshot([]*model.FlagState{parent, child})).Evaluate("child", NewContext("u"))
		if r.Reason != ReasonFallthrough || r.Variant != "on" {
			t.Errorf("got %+v", r)
		}
	})
	t.Run("parent off", func(t *testing.T) {
		p := boolState("parent")
		p.Config.Enabled = false
		r := New(snapshot([]*model.FlagState{p, child})).Evaluate("child", NewContext("u"))
		if r.Reason != ReasonPrerequisiteFailed || r.PrerequisiteKey != "parent" || r.Variant != "off" {
			t.Errorf("got %+v", r)
		}
	})
	t.Run("parent serves other variant", func(t *testing.T) {
		p := boolState("parent")
		p.Config.Fallthrough = model.Serve{Variant: "off"}
		r := New(snapshot([]*model.FlagState{p, child})).Evaluate("child", NewContext("u"))
		if r.Reason != ReasonPrerequisiteFailed {
			t.Errorf("got %+v", r)
		}
	})
	t.Run("parent missing", func(t *testing.T) {
		r := New(snapshot([]*model.FlagState{child})).Evaluate("child", NewContext("u"))
		if r.Reason != ReasonPrerequisiteFailed || r.Variant != "off" {
			t.Errorf("got %+v", r)
		}
	})
	t.Run("child off ignores prerequisites", func(t *testing.T) {
		c := boolState("child")
		c.Config.Enabled = false
		c.Config.Prerequisites = child.Config.Prerequisites
		r := New(snapshot([]*model.FlagState{c})).Evaluate("child", NewContext("u"))
		if r.Reason != ReasonOff {
			t.Errorf("got %+v", r)
		}
	})
	t.Run("cycle", func(t *testing.T) {
		a, b, c := boolState("a"), boolState("b"), boolState("c")
		a.Config.Prerequisites = []model.Prerequisite{{Flag: "b", Variant: "on"}}
		b.Config.Prerequisites = []model.Prerequisite{{Flag: "c", Variant: "on"}}
		c.Config.Prerequisites = []model.Prerequisite{{Flag: "a", Variant: "on"}}
		r := New(snapshot([]*model.FlagState{a, b, c})).Evaluate("a", NewContext("u"))
		if r.Error != ErrFlagCycle || !strings.Contains(r.Message, "a -> b -> c -> a") {
			t.Errorf("got %+v", r)
		}
	})
	t.Run("depth limit", func(t *testing.T) {
		var flags []*model.FlagState
		for i := 0; i < DefaultMaxDepth+2; i++ {
			f := boolState(fmt.Sprintf("f%d", i))
			if i > 0 {
				f.Config.Prerequisites = []model.Prerequisite{{Flag: fmt.Sprintf("f%d", i-1), Variant: "on"}}
			}
			flags = append(flags, f)
		}
		r := New(snapshot(flags)).Evaluate(fmt.Sprintf("f%d", DefaultMaxDepth+1), NewContext("u"))
		if r.Error != ErrDepthExceeded {
			t.Errorf("got %+v", r)
		}
	})
	t.Run("diamond is not a cycle", func(t *testing.T) {
		root, l, r := boolState("root"), boolState("left"), boolState("right")
		l.Config.Prerequisites = []model.Prerequisite{{Flag: "root", Variant: "on"}}
		r.Config.Prerequisites = []model.Prerequisite{{Flag: "root", Variant: "on"}}
		top := boolState("top")
		top.Config.Prerequisites = []model.Prerequisite{{Flag: "left", Variant: "on"}, {Flag: "right", Variant: "on"}}
		res := New(snapshot([]*model.FlagState{root, l, r, top})).Evaluate("top", NewContext("u"))
		if res.Failed() || res.Variant != "on" {
			t.Errorf("got %+v", res)
		}
	})
}

func TestSegments(t *testing.T) {
	beta := &model.Segment{
		Key: "beta", Included: []string{"inc"}, Excluded: []string{"exc"},
		Rules: []model.SegmentRule{{ID: "staff", Clauses: []model.Clause{clause("email", model.OpEndsWith, "@corp.test")}}},
	}
	half := &model.Segment{Key: "half", Rules: []model.SegmentRule{{ID: "all", Weight: ptr(50000), Clauses: []model.Clause{clause("key", model.OpExists)}}}}
	nested := &model.Segment{Key: "nested", Rules: []model.SegmentRule{{ID: "n", Clauses: []model.Clause{clause("", model.OpInSegment, "beta")}}}}

	flag := func(op model.Operator, seg string) *model.FlagState {
		f := boolState("f")
		f.Config.Fallthrough = model.Serve{Variant: "off"}
		f.Config.Rules = []model.Rule{rule("seg", "on", clause("", op, seg))}
		return f
	}
	tests := []struct {
		name string
		op   model.Operator
		seg  string
		ctx  Context
		want bool
	}{
		{"included", model.OpInSegment, "beta", NewContext("inc"), true},
		{"excluded beats rule", model.OpInSegment, "beta", NewContext("exc").With("email", "x@corp.test"), false},
		{"rule match", model.OpInSegment, "beta", NewContext("u").With("email", "x@corp.test"), true},
		{"no match", model.OpInSegment, "beta", NewContext("u"), false},
		{"not_in_segment", model.OpNotInSegment, "beta", NewContext("u"), true},
		{"not_in_segment member", model.OpNotInSegment, "beta", NewContext("inc"), false},
		{"unknown segment", model.OpInSegment, "ghost", NewContext("inc"), false},
		{"nested", model.OpInSegment, "nested", NewContext("inc"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ev := New(snapshot([]*model.FlagState{flag(tt.op, tt.seg)}, beta, half, nested))
			r := ev.Evaluate("f", tt.ctx)
			if got := r.Variant == "on"; got != tt.want {
				t.Errorf("in = %v, want %v (%+v)", got, tt.want, r)
			}
		})
	}

	t.Run("weighted rule includes about half", func(t *testing.T) {
		ev := New(snapshot([]*model.FlagState{flag(model.OpInSegment, "half")}, half))
		in := 0
		const n = 20000
		for i := 0; i < n; i++ {
			if ev.Evaluate("f", NewContext(fmt.Sprintf("u%d", i))).Variant == "on" {
				in++
			}
		}
		if frac := float64(in) / n; math.Abs(frac-0.5) > 0.02 {
			t.Errorf("weighted segment included %.3f, want ~0.5", frac)
		}
	})

	t.Run("cycle", func(t *testing.T) {
		a := &model.Segment{Key: "a", Rules: []model.SegmentRule{{ID: "x", Clauses: []model.Clause{clause("", model.OpInSegment, "b")}}}}
		b := &model.Segment{Key: "b", Rules: []model.SegmentRule{{ID: "y", Clauses: []model.Clause{clause("", model.OpInSegment, "a")}}}}
		r := New(snapshot([]*model.FlagState{flag(model.OpInSegment, "a")}, a, b)).Evaluate("f", NewContext("u"))
		if r.Error != ErrSegmentCycle {
			t.Errorf("got %+v", r)
		}
	})

	t.Run("segment cycle in prerequisite poisons dependent", func(t *testing.T) {
		a := &model.Segment{Key: "a", Rules: []model.SegmentRule{{ID: "x", Clauses: []model.Clause{clause("", model.OpInSegment, "a")}}}}
		dep := boolState("dep")
		dep.Config.Prerequisites = []model.Prerequisite{{Flag: "f", Variant: "on"}}
		r := New(snapshot([]*model.FlagState{flag(model.OpInSegment, "a"), dep}, a)).Evaluate("dep", NewContext("u"))
		if r.Error != ErrSegmentCycle {
			t.Errorf("got %+v", r)
		}
	})
}

func TestRollouts(t *testing.T) {
	f := colorState("exp")
	f.Config.Fallthrough = model.Serve{Rollout: &model.Rollout{Experiment: true, Variants: []model.WeightedVariant{
		{Variant: "red", Weight: 20000},
		{Variant: "green", Weight: 30000},
		{Variant: "blue", Weight: 50000},
	}}}
	ev := New(snapshot([]*model.FlagState{f}))

	t.Run("distribution", func(t *testing.T) {
		counts := map[string]int{}
		const n = 50000
		for i := 0; i < n; i++ {
			r := ev.Evaluate("exp", NewContext(fmt.Sprintf("user-%d", i)))
			if !r.InExperiment {
				t.Fatalf("experiment rollout result not marked in experiment: %+v", r)
			}
			counts[r.Variant]++
		}
		for variant, want := range map[string]float64{"red": 0.2, "green": 0.3, "blue": 0.5} {
			if got := float64(counts[variant]) / n; math.Abs(got-want) > 0.01 {
				t.Errorf("%s: %.3f, want %.2f", variant, got, want)
			}
		}
	})

	t.Run("sticky", func(t *testing.T) {
		ctx := NewContext("sticky-user").With("plan", "free")
		first := ev.Evaluate("exp", ctx).Variant
		for i := 0; i < 10; i++ {
			if got := ev.Evaluate("exp", ctx.With("plan", fmt.Sprint(i))).Variant; got != first {
				t.Fatalf("variant changed from %q to %q when an unrelated attribute changed", first, got)
			}
		}
	})

	t.Run("bucket by attribute", func(t *testing.T) {
		g := colorState("org-rollout")
		g.Config.Fallthrough = model.Serve{Rollout: &model.Rollout{BucketBy: "org", Variants: []model.WeightedVariant{
			{Variant: "red", Weight: 50000}, {Variant: "green", Weight: 50000},
		}}}
		ev := New(snapshot([]*model.FlagState{g}))
		for org := 0; org < 50; org++ {
			want := ev.Evaluate("org-rollout", NewContext("first").With("org", fmt.Sprint(org))).Variant
			for u := 0; u < 5; u++ {
				got := ev.Evaluate("org-rollout", NewContext(fmt.Sprintf("u%d", u)).With("org", fmt.Sprint(org))).Variant
				if got != want {
					t.Fatalf("org %d: users disagree (%q vs %q)", org, got, want)
				}
			}
		}
	})

	t.Run("missing bucket attribute", func(t *testing.T) {
		g := colorState("org-rollout")
		g.Config.Fallthrough = model.Serve{Rollout: &model.Rollout{BucketBy: "org", Experiment: true, Variants: []model.WeightedVariant{
			{Variant: "green", Weight: 50000}, {Variant: "blue", Weight: 50000},
		}}}
		r := New(snapshot([]*model.FlagState{g})).Evaluate("org-rollout", NewContext("u"))
		if r.Variant != "green" || r.InExperiment {
			t.Errorf("context without the bucket attribute should get the first slice outside the experiment, got %+v", r)
		}
	})

	t.Run("salt reshuffles", func(t *testing.T) {
		g := colorState("exp")
		g.Config.Fallthrough = f.Config.Fallthrough
		g.Config.Salt = "restarted"
		ev2 := New(snapshot([]*model.FlagState{g}))
		changed := 0
		for i := 0; i < 1000; i++ {
			ctx := NewContext(fmt.Sprintf("user-%d", i))
			if ev.Evaluate("exp", ctx).Variant != ev2.Evaluate("exp", ctx).Variant {
				changed++
			}
		}
		if changed < 300 {
			t.Errorf("changing the salt moved only %d of 1000 users", changed)
		}
	})
}

func TestExplainTrace(t *testing.T) {
	f := boolState("f")
	f.Config.Rules = []model.Rule{
		rule("nz", "off", clause("country", model.OpEquals, "NZ")),
		rule("beta", "on", clause("", model.OpInSegment, "beta")),
	}
	seg := &model.Segment{Key: "beta", Included: []string{"u"}}
	ev := New(snapshot([]*model.FlagState{f}, seg))
	r := ev.Explain("f", NewContext("u"))
	joined := strings.Join(r.Trace, "\n")
	for _, want := range []string{`flag "f"`, `rule 0 (nz) did not match`, `segment "beta": context explicitly included`, `rule 1 (beta) matched`} {
		if !strings.Contains(joined, want) {
			t.Errorf("trace missing %q:\n%s", want, joined)
		}
	}
	if plain := ev.Evaluate("f", NewContext("u")); plain.Trace != nil {
		t.Error("Evaluate should not record a trace")
	}
}

func TestEvaluateAllIsSorted(t *testing.T) {
	ev := New(snapshot([]*model.FlagState{boolState("c"), boolState("a"), boolState("b")}))
	var keys []string
	for _, r := range ev.EvaluateAll(NewContext("u")) {
		keys = append(keys, r.FlagKey)
	}
	if strings.Join(keys, ",") != "a,b,c" {
		t.Errorf("keys = %v", keys)
	}
}

func benchSnapshot() *model.Snapshot {
	seg := &model.Segment{Key: "big"}
	for i := 0; i < 10000; i++ {
		seg.Included = append(seg.Included, fmt.Sprintf("member-%d", i))
	}
	f := colorState("bench")
	for i := 0; i < 10; i++ {
		f.Config.Rules = append(f.Config.Rules, rule(fmt.Sprintf("r%d", i), "green",
			clause("country", model.OpEquals, fmt.Sprintf("C%d", i)),
			clause("email", model.OpEndsWith, "@example.com")))
	}
	f.Config.Rules = append(f.Config.Rules, rule("seg", "blue", clause("", model.OpInSegment, "big")))
	f.Config.Fallthrough = model.Serve{Rollout: &model.Rollout{Variants: []model.WeightedVariant{
		{Variant: "red", Weight: 50000}, {Variant: "green", Weight: 50000},
	}}}
	return snapshot([]*model.FlagState{f}, seg)
}

func BenchmarkEvaluateFallthroughRollout(b *testing.B) {
	ev := New(benchSnapshot())
	ctx := NewContext("user-1").With("country", "NZ").With("email", "a@example.org")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ev.Evaluate("bench", ctx)
	}
}

func BenchmarkEvaluateSegmentMember(b *testing.B) {
	ev := New(benchSnapshot())
	ctx := NewContext("member-9999").With("country", "NZ")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ev.Evaluate("bench", ctx)
	}
}

func BenchmarkEvaluateParallel(b *testing.B) {
	ev := New(benchSnapshot())
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		ctx := NewContext("user-1").With("country", "NZ")
		for pb.Next() {
			ev.Evaluate("bench", ctx)
		}
	})
}

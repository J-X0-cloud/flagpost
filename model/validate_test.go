package model

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func boolFlag() *Flag {
	return &Flag{
		Key:  "new-checkout",
		Name: "New checkout",
		Kind: KindBoolean,
		Variants: []Variant{
			{Key: "on", Value: json.RawMessage(`true`)},
			{Key: "off", Value: json.RawMessage(`false`)},
		},
	}
}

func problemFields(t *testing.T, err error) []string {
	t.Helper()
	if err == nil {
		return nil
	}
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("error %v is not a *ValidationError", err)
	}
	var out []string
	for _, p := range ve.Problems {
		out = append(out, p.Field)
	}
	return out
}

func assertFields(t *testing.T, err error, want ...string) {
	t.Helper()
	got := problemFields(t, err)
	if len(want) == 0 {
		if len(got) != 0 {
			t.Fatalf("unexpected problems: %v", err)
		}
		return
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected a problem at %q, got %v (%v)", w, got, err)
		}
	}
}

func TestValidKey(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"a", true},
		{"new-checkout", true},
		{"Team.Feature_2", true},
		{"9lives", true},
		{"", false},
		{"-leading", false},
		{".leading", false},
		{"has space", false},
		{"slash/es", false},
		{strings.Repeat("a", 128), true},
		{strings.Repeat("a", 129), false},
	}
	for _, tt := range tests {
		if got := ValidKey(tt.in); got != tt.want {
			t.Errorf("ValidKey(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestFlagValidate(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(f *Flag)
		fields []string
	}{
		{"valid", func(*Flag) {}, nil},
		{"bad key", func(f *Flag) { f.Key = "no spaces" }, []string{"key"}},
		{"blank name", func(f *Flag) { f.Name = "  " }, []string{"name"}},
		{"bad kind", func(f *Flag) { f.Kind = "date" }, []string{"kind"}},
		{"no variants", func(f *Flag) { f.Variants = nil }, []string{"variants"}},
		{"duplicate variant key", func(f *Flag) { f.Variants[1].Key = "on" }, []string{"variants[1].key"}},
		{"duplicate variant value", func(f *Flag) { f.Variants[1].Value = json.RawMessage(`true`) }, []string{"variants[1].value"}},
		{"wrong value type", func(f *Flag) { f.Variants[0].Value = json.RawMessage(`"yes"`) }, []string{"variants[0].value"}},
		{"missing value", func(f *Flag) { f.Variants[0].Value = nil }, []string{"variants[0].value"}},
		{"invalid json", func(f *Flag) { f.Variants[0].Value = json.RawMessage(`{`) }, []string{"variants[0].value"}},
		{"three boolean variants", func(f *Flag) {
			f.Variants = append(f.Variants, Variant{Key: "maybe", Value: json.RawMessage(`true`)})
		}, []string{"variants"}},
		{"blank tag", func(f *Flag) { f.Tags = []string{"ok", ""} }, []string{"tags[1]"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := boolFlag()
			tt.mutate(f)
			assertFields(t, f.Validate(), tt.fields...)
		})
	}
}

func TestFlagValidateKinds(t *testing.T) {
	tests := []struct {
		kind  FlagKind
		value string
		ok    bool
	}{
		{KindString, `"blue"`, true},
		{KindString, `3`, false},
		{KindNumber, `3.5`, true},
		{KindNumber, `"3.5"`, false},
		{KindJSON, `{"a":[1,2]}`, true},
		{KindJSON, `null`, true},
	}
	for _, tt := range tests {
		f := &Flag{Key: "k", Name: "n", Kind: tt.kind, Variants: []Variant{{Key: "a", Value: json.RawMessage(tt.value)}}}
		err := f.Validate()
		if (err == nil) != tt.ok {
			t.Errorf("kind %s value %s: err=%v, want ok=%v", tt.kind, tt.value, err, tt.ok)
		}
	}
}

func TestJSONVariantsMayRepeatValues(t *testing.T) {
	f := &Flag{Key: "cfg", Name: "Config", Kind: KindJSON, Variants: []Variant{
		{Key: "a", Value: json.RawMessage(`{"x":1,"y":2}`)},
		{Key: "b", Value: json.RawMessage(`{"y":2,"x":1}`)},
	}}
	if err := f.Validate(); err != nil {
		t.Fatalf("json flags may repeat values: %v", err)
	}
}

func weight(n int) *int { return &n }

func TestFlagConfigValidate(t *testing.T) {
	base := func() FlagConfig {
		return FlagConfig{
			FlagKey:     "new-checkout",
			Environment: "production",
			OffVariant:  "off",
			Fallthrough: Serve{Variant: "on"},
		}
	}
	rollout := func(ws ...WeightedVariant) Serve { return Serve{Rollout: &Rollout{Variants: ws}} }
	tests := []struct {
		name   string
		mutate func(c *FlagConfig)
		fields []string
	}{
		{"valid", func(*FlagConfig) {}, nil},
		{"wrong flag", func(c *FlagConfig) { c.FlagKey = "other" }, []string{"flagKey"}},
		{"missing off variant", func(c *FlagConfig) { c.OffVariant = "" }, []string{"offVariant"}},
		{"unknown off variant", func(c *FlagConfig) { c.OffVariant = "nope" }, []string{"offVariant"}},
		{"both variant and rollout", func(c *FlagConfig) {
			c.Fallthrough = Serve{Variant: "on", Rollout: &Rollout{}}
		}, []string{"fallthrough"}},
		{"valid rollout", func(c *FlagConfig) {
			c.Fallthrough = rollout(WeightedVariant{"on", 25000}, WeightedVariant{"off", 75000})
		}, nil},
		{"rollout under 100%", func(c *FlagConfig) {
			c.Fallthrough = rollout(WeightedVariant{"on", 25000}, WeightedVariant{"off", 70000})
		}, []string{"fallthrough.rollout.variants"}},
		{"negative weight", func(c *FlagConfig) {
			c.Fallthrough = rollout(WeightedVariant{"on", -1}, WeightedVariant{"off", 100001})
		}, []string{"fallthrough.rollout.variants[0].weight", "fallthrough.rollout.variants[1].weight"}},
		{"repeated rollout variant", func(c *FlagConfig) {
			c.Fallthrough = rollout(WeightedVariant{"on", 50000}, WeightedVariant{"on", 50000})
		}, []string{"fallthrough.rollout.variants[1].variant"}},
		{"empty rollout", func(c *FlagConfig) { c.Fallthrough = rollout() }, []string{"fallthrough.rollout.variants"}},
		{"target without values", func(c *FlagConfig) {
			c.Targets = []Target{{Variant: "on"}}
		}, []string{"targets[0].values"}},
		{"context in two targets", func(c *FlagConfig) {
			c.Targets = []Target{{Variant: "on", Values: []string{"u1"}}, {Variant: "off", Values: []string{"u1"}}}
		}, []string{"targets[1].values"}},
		{"rule missing id and clauses", func(c *FlagConfig) {
			c.Rules = []Rule{{Serve: Serve{Variant: "on"}}}
		}, []string{"rules[0].id", "rules[0].clauses"}},
		{"duplicate rule ids", func(c *FlagConfig) {
			cl := []Clause{{Attribute: "country", Operator: OpEquals, Values: []any{"NZ"}}}
			c.Rules = []Rule{{ID: "r", Clauses: cl, Serve: Serve{Variant: "on"}}, {ID: "r", Clauses: cl, Serve: Serve{Variant: "on"}}}
		}, []string{"rules[1].id"}},
		{"bad clause values", func(c *FlagConfig) {
			c.Rules = []Rule{{ID: "r", Serve: Serve{Variant: "on"}, Clauses: []Clause{
				{Attribute: "", Operator: OpEquals, Values: []any{"x"}},
				{Attribute: "a", Operator: "approx", Values: []any{"x"}},
				{Attribute: "a", Operator: OpGreater, Values: []any{"ten"}},
				{Attribute: "a", Operator: OpMatches, Values: []any{"("}},
				{Attribute: "a", Operator: OpSemverGt, Values: []any{"1.x"}},
				{Attribute: "a", Operator: OpBefore, Values: []any{"yesterday"}},
				{Attribute: "a", Operator: OpContains, Values: []any{3.0}},
				{Attribute: "a", Operator: OpEquals},
				{Operator: OpInSegment, Values: []any{"bad key!"}},
			}}}
		}, []string{
			"rules[0].clauses[0].attribute",
			"rules[0].clauses[1].operator",
			"rules[0].clauses[2].values[0]",
			"rules[0].clauses[3].values[0]",
			"rules[0].clauses[4].values[0]",
			"rules[0].clauses[5].values[0]",
			"rules[0].clauses[6].values[0]",
			"rules[0].clauses[7].values",
			"rules[0].clauses[8].values[0]",
		}},
		{"exists needs no values", func(c *FlagConfig) {
			c.Rules = []Rule{{ID: "r", Serve: Serve{Variant: "on"}, Clauses: []Clause{{Attribute: "email", Operator: OpExists}}}}
		}, nil},
		{"self prerequisite", func(c *FlagConfig) {
			c.Prerequisites = []Prerequisite{{Flag: "new-checkout", Variant: "on"}}
		}, []string{"prerequisites[0].flag"}},
		{"duplicate prerequisite", func(c *FlagConfig) {
			c.Prerequisites = []Prerequisite{{Flag: "a", Variant: "x"}, {Flag: "a", Variant: "y"}}
		}, []string{"prerequisites[1].flag"}},
		{"prerequisite without variant", func(c *FlagConfig) {
			c.Prerequisites = []Prerequisite{{Flag: "a"}}
		}, []string{"prerequisites[0].variant"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := base()
			tt.mutate(&c)
			assertFields(t, c.Validate(boolFlag()), tt.fields...)
		})
	}
}

func TestSegmentValidate(t *testing.T) {
	tests := []struct {
		name   string
		seg    Segment
		fields []string
	}{
		{"valid", Segment{Key: "beta", Name: "Beta", Included: []string{"u1"}}, nil},
		{"bad key", Segment{Key: "", Name: "Beta"}, []string{"key"}},
		{"no name", Segment{Key: "beta"}, []string{"name"}},
		{"both included and excluded", Segment{Key: "beta", Name: "B", Included: []string{"u1"}, Excluded: []string{"u1"}}, []string{"excluded[0]"}},
		{"self reference", Segment{Key: "beta", Name: "B", Rules: []SegmentRule{{ID: "r", Clauses: []Clause{
			{Operator: OpInSegment, Values: []any{"beta"}},
		}}}}, []string{"rules[0].clauses[0]"}},
		{"weight out of range", Segment{Key: "beta", Name: "B", Rules: []SegmentRule{{ID: "r", Weight: weight(100001), Clauses: []Clause{
			{Attribute: "plan", Operator: OpEquals, Values: []any{"pro"}},
		}}}}, []string{"rules[0].weight"}},
		{"duplicate rule ids", Segment{Key: "beta", Name: "B", Rules: []SegmentRule{
			{ID: "r", Clauses: []Clause{{Attribute: "a", Operator: OpExists}}},
			{ID: "r", Clauses: []Clause{{Attribute: "a", Operator: OpExists}}},
		}}, []string{"rules[1].id"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertFields(t, tt.seg.Validate(), tt.fields...)
		})
	}
}

func TestSegmentRefs(t *testing.T) {
	c := FlagConfig{Rules: []Rule{
		{Clauses: []Clause{{Operator: OpInSegment, Values: []any{"a", "b"}}, {Attribute: "x", Operator: OpEquals, Values: []any{"a"}}}},
		{Clauses: []Clause{{Operator: OpNotInSegment, Values: []any{"b", "c"}}}},
	}}
	got := strings.Join(c.SegmentRefs(), ",")
	if got != "a,b,c" {
		t.Errorf("SegmentRefs = %s, want a,b,c", got)
	}
}

func TestParseTimeValue(t *testing.T) {
	want := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		in any
		ok bool
	}{
		{"2026-03-01T12:00:00Z", true},
		{"2026-03-01T13:00:00+01:00", true},
		{float64(want.UnixMilli()), true},
		{want.UnixMilli(), true},
		{"March 1st", false},
		{true, false},
		{nil, false},
	}
	for _, tt := range tests {
		got, ok := ParseTimeValue(tt.in)
		if ok != tt.ok {
			t.Errorf("ParseTimeValue(%v) ok = %v, want %v", tt.in, ok, tt.ok)
			continue
		}
		if ok && !got.Equal(want) {
			t.Errorf("ParseTimeValue(%v) = %v, want %v", tt.in, got, want)
		}
	}
}

func TestDefaultConfig(t *testing.T) {
	f := boolFlag()
	c := DefaultConfig(f, "staging")
	if c.Enabled || c.OffVariant != "off" || c.Fallthrough.Variant != "on" || c.Salt != f.Key {
		t.Errorf("unexpected default config: %+v", c)
	}
	if err := c.Validate(f); err != nil {
		t.Errorf("default config does not validate: %v", err)
	}
}

func TestValidationErrorMessage(t *testing.T) {
	err := (&Flag{}).Validate()
	if err == nil || !strings.Contains(err.Error(), "key:") {
		t.Fatalf("unexpected error text: %v", err)
	}
}

func TestOperators(t *testing.T) {
	for _, op := range Operators() {
		if !op.Valid() {
			t.Errorf("listed operator %q is not valid", op)
		}
	}
	if len(Operators()) != len(operatorClasses) {
		t.Errorf("Operators() lists %d operators, class table has %d", len(Operators()), len(operatorClasses))
	}
}

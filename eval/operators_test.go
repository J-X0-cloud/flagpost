package eval

import (
	"encoding/json"
	"fmt"
	"regexp"
	"testing"

	"github.com/J-X0-cloud/flagpost/model"
)

func TestMatchValue(t *testing.T) {
	tests := []struct {
		name string
		op   model.Operator
		attr any
		want any
		ok   bool
	}{
		{"eq strings", model.OpEquals, "NZ", "NZ", true},
		{"eq case sensitive", model.OpEquals, "nz", "NZ", false},
		{"eq numbers", model.OpEquals, float64(3), float64(3), true},
		{"eq number vs string", model.OpEquals, float64(3), "3", false},
		{"eq bools", model.OpEquals, true, true, true},
		{"eq int normalised", model.OpEquals, 3, float64(3), true},
		{"eq objects", model.OpEquals, map[string]any{"a": 1.0}, map[string]any{"a": 1.0}, true},
		{"eq null", model.OpEquals, nil, nil, true},
		{"contains", model.OpContains, "hello world", "lo w", true},
		{"contains miss", model.OpContains, "hello", "xyz", false},
		{"contains non-string", model.OpContains, float64(12), "1", false},
		{"starts_with", model.OpStartsWith, "admin@example.com", "admin@", true},
		{"ends_with", model.OpEndsWith, "admin@example.com", "@example.com", true},
		{"ends_with miss", model.OpEndsWith, "admin@example.org", "@example.com", false},
		{"matches", model.OpMatches, "build-1234", `^build-\d+$`, true},
		{"matches miss", model.OpMatches, "build-x", `^build-\d+$`, false},
		{"matches invalid pattern", model.OpMatches, "x", `(`, false},
		{"gt", model.OpGreater, float64(10), float64(5), true},
		{"gt equal", model.OpGreater, float64(5), float64(5), false},
		{"gte equal", model.OpGreaterEq, float64(5), float64(5), true},
		{"lt", model.OpLess, float64(1), float64(5), true},
		{"lte", model.OpLessEq, float64(5), float64(5), true},
		{"lt string attr", model.OpLess, "1", float64(5), false},
		{"lt json.Number", model.OpLess, json.Number("1.5"), float64(2), true},
		{"before", model.OpBefore, "2026-01-01T00:00:00Z", "2026-06-01T00:00:00Z", true},
		{"before millis", model.OpBefore, float64(0), "2026-06-01T00:00:00Z", true},
		{"after", model.OpAfter, "2026-07-01T00:00:00Z", "2026-06-01T00:00:00Z", true},
		{"after unparsable", model.OpAfter, "soon", "2026-06-01T00:00:00Z", false},
		{"semver_eq", model.OpSemverEq, "1.2.0", "1.2", true},
		{"semver_gt", model.OpSemverGt, "1.10.0", "1.9.9", true},
		{"semver_gt prerelease", model.OpSemverGt, "2.0.0-rc.1", "2.0.0", false},
		{"semver_lt", model.OpSemverLt, "2.0.0-rc.1", "2.0.0", true},
		{"semver invalid attr", model.OpSemverLt, "latest", "2.0.0", false},
		{"semver non-string", model.OpSemverEq, float64(1), "1.0.0", false},
		{"unknown operator", model.Operator("near"), "a", "a", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchValue(tt.op, normalize(tt.attr), tt.want); got != tt.ok {
				t.Errorf("matchValue(%s, %v, %v) = %v, want %v", tt.op, tt.attr, tt.want, got, tt.ok)
			}
		})
	}
}

func TestRegexCacheBounded(t *testing.T) {
	c := &regexCache{limit: 4, m: map[string]*regexp.Regexp{}}
	for i := 0; i < 10; i++ {
		if c.get(fmt.Sprintf("^a{%d}$", i)) == nil {
			t.Fatalf("pattern %d failed to compile", i)
		}
		if len(c.m) > 4 {
			t.Fatalf("cache grew to %d entries", len(c.m))
		}
	}
	if c.get("(") != nil {
		t.Error("invalid pattern should yield nil")
	}
	if _, cached := c.m["("]; !cached {
		t.Error("invalid pattern should be cached")
	}
}

func TestContextGet(t *testing.T) {
	ctx := Context{Key: "u1", Attributes: map[string]any{
		"plan":         "pro",
		"seats":        12,
		"address":      map[string]any{"country": "NZ", "geo": map[string]any{"city": "Wellington"}},
		"labels":       map[string]string{"team": "core"},
		"dotted.attr":  "literal",
		"dotted":       map[string]any{"attr": "nested"},
		"groups":       []string{"a", "b"},
		"explicitNull": nil,
	}}
	tests := []struct {
		name string
		want any
		ok   bool
	}{
		{"key", "u1", true},
		{"plan", "pro", true},
		{"seats", float64(12), true},
		{"address.country", "NZ", true},
		{"address.geo.city", "Wellington", true},
		{"labels.team", "core", true},
		{"dotted.attr", "literal", true},
		{"address.missing", nil, false},
		{"plan.sub", nil, false},
		{"missing", nil, false},
		{"explicitNull", nil, true},
	}
	for _, tt := range tests {
		got, ok := ctx.Get(tt.name)
		if ok != tt.ok || !valuesEqual(got, tt.want) {
			t.Errorf("Get(%q) = %v, %v; want %v, %v", tt.name, got, ok, tt.want, tt.ok)
		}
	}
	groups, _ := ctx.Get("groups")
	if l, ok := groups.([]any); !ok || len(l) != 2 {
		t.Errorf("typed slices should normalise to []any, got %T", groups)
	}
	if _, ok := (Context{}).Get("key"); ok {
		t.Error("empty key should be reported as missing")
	}
}

func TestContextWithDoesNotMutate(t *testing.T) {
	base := NewContext("u1").With("plan", "free")
	derived := base.With("plan", "pro")
	if v, _ := base.Get("plan"); v != "free" {
		t.Errorf("base was mutated: %v", v)
	}
	if v, _ := derived.Get("plan"); v != "pro" {
		t.Errorf("derived = %v", v)
	}
}

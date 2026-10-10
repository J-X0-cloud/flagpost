package audit

import (
	"encoding/json"
	"strings"
	"testing"
)

func changesString(cs []Change) string {
	var parts []string
	for _, c := range cs {
		s := c.Op + " " + c.Path
		if c.Old != nil {
			s += " " + string(c.Old)
		}
		if c.New != nil {
			s += " -> " + string(c.New)
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "\n")
}

func mustJSON(s string) json.RawMessage { return json.RawMessage(s) }

func TestDiff(t *testing.T) {
	tests := []struct {
		name          string
		before, after any
		opts          DiffOptions
		want          string
	}{
		{
			name:   "identical",
			before: mustJSON(`{"a":1,"b":[1,2]}`),
			after:  mustJSON(`{"b":[1,2],"a":1}`),
			want:   "",
		},
		{
			name:   "creation",
			before: nil,
			after:  mustJSON(`{"a":1}`),
			want:   `add $ -> {"a":1}`,
		},
		{
			name:   "deletion",
			before: mustJSON(`{"a":1}`),
			after:  nil,
			want:   `remove $ {"a":1}`,
		},
		{
			name:   "scalar change and new field",
			before: mustJSON(`{"enabled":false,"name":"x"}`),
			after:  mustJSON(`{"enabled":true,"name":"x","description":"d"}`),
			want:   "add description -> \"d\"\nreplace enabled false -> true",
		},
		{
			name:   "nested field removed",
			before: mustJSON(`{"fallthrough":{"variant":"on"}}`),
			after:  mustJSON(`{"fallthrough":{}}`),
			want:   `remove fallthrough.variant "on"`,
		},
		{
			name:   "null and missing are equivalent",
			before: mustJSON(`{"rollout":null}`),
			after:  mustJSON(`{}`),
			want:   "",
		},
		{
			name:   "type change",
			before: mustJSON(`{"v":[1]}`),
			after:  mustJSON(`{"v":{"x":1}}`),
			want:   `replace v [1] -> {"x":1}`,
		},
		{
			name:   "positional array",
			before: mustJSON(`{"tags":["a","b","c"]}`),
			after:  mustJSON(`{"tags":["a","x"]}`),
			want:   "replace tags[1] \"b\" -> \"x\"\nremove tags[2] \"c\"",
		},
		{
			name:   "keyed array insert at top",
			before: mustJSON(`{"rules":[{"id":"a","v":1},{"id":"b","v":2}]}`),
			after:  mustJSON(`{"rules":[{"id":"new","v":0},{"id":"a","v":1},{"id":"b","v":2}]}`),
			want:   `add rules[id=new] -> {"id":"new","v":0}`,
		},
		{
			name:   "keyed array edit and reorder",
			before: mustJSON(`{"rules":[{"id":"a","v":1},{"id":"b","v":2}]}`),
			after:  mustJSON(`{"rules":[{"id":"b","v":3},{"id":"a","v":1}]}`),
			want:   "replace rules[id=b].v 2 -> 3\nreplace rules[*] [\"a\",\"b\"] -> [\"b\",\"a\"]",
		},
		{
			name:   "keyed by key field",
			before: mustJSON(`{"variants":[{"key":"on","value":true}]}`),
			after:  mustJSON(`{"variants":[]}`),
			want:   `remove variants[key=on] {"key":"on","value":true}`,
		},
		{
			name:   "duplicate ids fall back to positions",
			before: mustJSON(`[{"id":"a","v":1},{"id":"a","v":2}]`),
			after:  mustJSON(`[{"id":"a","v":1},{"id":"a","v":3}]`),
			want:   "replace $[1].v 2 -> 3",
		},
		{
			name:   "ignored fields",
			before: mustJSON(`{"version":1,"updatedAt":"x","enabled":false}`),
			after:  mustJSON(`{"version":2,"updatedAt":"y","enabled":false}`),
			opts:   DiffOptions{Ignore: []string{"version", "updatedAt"}},
			want:   "",
		},
		{
			name:   "structs are encoded",
			before: struct{ A int }{1},
			after:  struct{ A int }{2},
			want:   "replace A 1 -> 2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Diff(tt.before, tt.after, tt.opts)
			if err != nil {
				t.Fatal(err)
			}
			if s := changesString(got); s != tt.want {
				t.Errorf("diff mismatch\n got: %s\nwant: %s", s, tt.want)
			}
		})
	}
}

func TestDiffNilPointer(t *testing.T) {
	var p *struct{ A int }
	got, err := Diff(p, struct{ A int }{1}, DiffOptions{})
	if err != nil || len(got) != 1 || got[0].Op != OpAdd {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestDiffInvalidJSON(t *testing.T) {
	if _, err := Diff(json.RawMessage(`{`), nil, DiffOptions{}); err == nil {
		t.Error("expected an error for invalid JSON")
	}
	if _, err := Diff(nil, func() {}, DiffOptions{}); err == nil {
		t.Error("expected an error for unencodable values")
	}
}

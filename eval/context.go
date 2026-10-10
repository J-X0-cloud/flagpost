package eval

import (
	"encoding/json"
	"strings"
)

// Context describes the subject a flag is evaluated for: usually a user,
// but equally an organisation, device or request. Key identifies it and is
// the default input to percentage rollouts; Attributes feed targeting rules.
type Context struct {
	Key        string         `json:"key"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

// NewContext returns a context with the given key and no attributes.
func NewContext(key string) Context { return Context{Key: key} }

// With returns a copy of c with attribute name set to value. The receiver is
// not modified, so contexts can be shared between goroutines and extended
// per request.
func (c Context) With(name string, value any) Context {
	attrs := make(map[string]any, len(c.Attributes)+1)
	for k, v := range c.Attributes {
		attrs[k] = v
	}
	attrs[name] = value
	return Context{Key: c.Key, Attributes: attrs}
}

// Get resolves an attribute reference. The reserved name "key" returns the
// context key. A name that is not a top-level attribute but contains dots is
// treated as a path into nested objects, so "address.country" finds
// {"address": {"country": "NZ"}}. A top-level attribute whose name contains
// a literal dot always wins over the path interpretation.
func (c Context) Get(name string) (any, bool) {
	if name == "key" {
		return c.Key, c.Key != ""
	}
	if v, ok := c.Attributes[name]; ok {
		return normalize(v), true
	}
	if !strings.Contains(name, ".") {
		return nil, false
	}
	var cur any = c.Attributes
	for _, part := range strings.Split(name, ".") {
		m, ok := asObject(cur)
		if !ok {
			return nil, false
		}
		cur, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return normalize(cur), true
}

func asObject(v any) (map[string]any, bool) {
	switch m := v.(type) {
	case map[string]any:
		return m, true
	case map[string]string:
		out := make(map[string]any, len(m))
		for k, s := range m {
			out[k] = s
		}
		return out, true
	}
	return nil, false
}

// normalize maps the many Go numeric types callers might use onto float64,
// and typed slices onto []any, so the operator code only has to deal with
// the shapes encoding/json produces.
func normalize(v any) any {
	switch t := v.(type) {
	case int:
		return float64(t)
	case int8:
		return float64(t)
	case int16:
		return float64(t)
	case int32:
		return float64(t)
	case int64:
		return float64(t)
	case uint:
		return float64(t)
	case uint8:
		return float64(t)
	case uint16:
		return float64(t)
	case uint32:
		return float64(t)
	case uint64:
		return float64(t)
	case float32:
		return float64(t)
	case json.Number:
		if f, err := t.Float64(); err == nil {
			return f
		}
		return t.String()
	case []string:
		out := make([]any, len(t))
		for i, s := range t {
			out[i] = s
		}
		return out
	case []int:
		out := make([]any, len(t))
		for i, n := range t {
			out[i] = float64(n)
		}
		return out
	case []float64:
		out := make([]any, len(t))
		for i, n := range t {
			out[i] = n
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = normalize(e)
		}
		return out
	}
	return v
}

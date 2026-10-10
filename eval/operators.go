package eval

import (
	"encoding/json"
	"math"
	"reflect"
	"regexp"
	"strings"
	"sync"

	"github.com/J-X0-cloud/flagpost/internal/semver"
	"github.com/J-X0-cloud/flagpost/model"
)

// matchValue applies a non-segment operator to a single attribute value and
// a single clause value. Type mismatches are never errors: they simply do
// not match, because contexts are supplied by applications and a rule must
// not take a service down when someone sends a number where a string was
// expected.
func matchValue(op model.Operator, attr, want any) bool {
	switch op {
	case model.OpEquals, model.OpIn, model.OpNotEquals:
		return valuesEqual(attr, want)
	case model.OpContains, model.OpStartsWith, model.OpEndsWith:
		a, ok1 := attr.(string)
		w, ok2 := want.(string)
		if !ok1 || !ok2 {
			return false
		}
		switch op {
		case model.OpContains:
			return strings.Contains(a, w)
		case model.OpStartsWith:
			return strings.HasPrefix(a, w)
		default:
			return strings.HasSuffix(a, w)
		}
	case model.OpMatches:
		a, ok1 := attr.(string)
		w, ok2 := want.(string)
		if !ok1 || !ok2 {
			return false
		}
		re := regexps.get(w)
		return re != nil && re.MatchString(a)
	case model.OpGreater, model.OpGreaterEq, model.OpLess, model.OpLessEq:
		a, ok1 := toNumber(attr)
		w, ok2 := toNumber(want)
		if !ok1 || !ok2 {
			return false
		}
		switch op {
		case model.OpGreater:
			return a > w
		case model.OpGreaterEq:
			return a >= w
		case model.OpLess:
			return a < w
		default:
			return a <= w
		}
	case model.OpBefore, model.OpAfter:
		a, ok1 := model.ParseTimeValue(attr)
		w, ok2 := model.ParseTimeValue(want)
		if !ok1 || !ok2 {
			return false
		}
		if op == model.OpBefore {
			return a.Before(w)
		}
		return a.After(w)
	case model.OpSemverEq, model.OpSemverGt, model.OpSemverLt:
		a, ok1 := toSemver(attr)
		w, ok2 := toSemver(want)
		if !ok1 || !ok2 {
			return false
		}
		c := a.Compare(w)
		switch op {
		case model.OpSemverEq:
			return c == 0
		case model.OpSemverGt:
			return c > 0
		default:
			return c < 0
		}
	}
	return false
}

func toNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, !math.IsNaN(t)
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	}
	return 0, false
}

func toSemver(v any) (semver.Version, bool) {
	s, ok := v.(string)
	if !ok {
		return semver.Version{}, false
	}
	ver, err := semver.Parse(s)
	return ver, err == nil
}

// valuesEqual compares two JSON-shaped values. Numbers compare by value
// regardless of their Go type, and objects and arrays compare structurally.
func valuesEqual(a, b any) bool {
	a, b = normalize(a), normalize(b)
	switch at := a.(type) {
	case string:
		bt, ok := b.(string)
		return ok && at == bt
	case float64:
		bt, ok := b.(float64)
		return ok && at == bt
	case bool:
		bt, ok := b.(bool)
		return ok && at == bt
	case nil:
		return b == nil
	}
	return reflect.DeepEqual(a, b)
}

// regexCache memoises compiled clause patterns. Patterns come from flag
// configuration, so the set is small and changes rarely; the cache is
// cleared wholesale if it ever grows past its limit, which keeps memory
// bounded for long-lived SDK processes that see many config revisions.
type regexCache struct {
	mu    sync.RWMutex
	limit int
	m     map[string]*regexp.Regexp
}

var regexps = &regexCache{limit: 512, m: map[string]*regexp.Regexp{}}

func (c *regexCache) get(pattern string) *regexp.Regexp {
	c.mu.RLock()
	re, ok := c.m[pattern]
	c.mu.RUnlock()
	if ok {
		return re
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		re = nil // cached as nil so invalid patterns are not recompiled
	}
	c.mu.Lock()
	if len(c.m) >= c.limit {
		c.m = map[string]*regexp.Regexp{}
	}
	c.m[pattern] = re
	c.mu.Unlock()
	return re
}

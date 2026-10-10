package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"

	"github.com/J-X0-cloud/flagpost/internal/semver"
)

// Problem is a single validation failure located by a JSON-ish field path.
type Problem struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// ValidationError collects every problem found while validating an object,
// so API clients can fix them all in one round trip.
type ValidationError struct {
	Problems []Problem `json:"problems"`
}

func (e *ValidationError) Error() string {
	parts := make([]string, len(e.Problems))
	for i, p := range e.Problems {
		parts[i] = p.Field + ": " + p.Message
	}
	return "validation failed: " + strings.Join(parts, "; ")
}

type problems []Problem

func (p *problems) add(field, format string, args ...any) {
	*p = append(*p, Problem{Field: field, Message: fmt.Sprintf(format, args...)})
}

func (p problems) err() error {
	if len(p) == 0 {
		return nil
	}
	return &ValidationError{Problems: p}
}

var keyPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// ValidKey reports whether s is acceptable as a flag, segment, environment
// or variant key: 1 to 128 characters of letters, digits, '.', '_' and '-',
// not starting with punctuation.
func ValidKey(s string) bool { return keyPattern.MatchString(s) }

// Validate checks the flag definition for internal consistency.
func (f *Flag) Validate() error {
	var p problems
	if !ValidKey(f.Key) {
		p.add("key", "must be 1-128 characters of letters, digits, '.', '_' or '-'")
	}
	if strings.TrimSpace(f.Name) == "" {
		p.add("name", "is required")
	}
	if !f.Kind.Valid() {
		p.add("kind", "must be one of boolean, string, number, json")
	}
	if len(f.Variants) == 0 {
		p.add("variants", "at least one variant is required")
	}
	seen := map[string]bool{}
	seenValues := map[string]string{}
	for i, v := range f.Variants {
		field := fmt.Sprintf("variants[%d]", i)
		if !ValidKey(v.Key) {
			p.add(field+".key", "is not a valid key")
		} else if seen[v.Key] {
			p.add(field+".key", "duplicate variant key %q", v.Key)
		}
		seen[v.Key] = true
		if msg := checkVariantValue(f.Kind, v.Value); msg != "" {
			p.add(field+".value", "%s", msg)
			continue
		}
		canon := canonicalJSON(v.Value)
		if other, dup := seenValues[canon]; dup && f.Kind != KindJSON {
			p.add(field+".value", "duplicates the value of variant %q", other)
		}
		seenValues[canon] = v.Key
	}
	if f.Kind == KindBoolean && len(f.Variants) != 2 {
		p.add("variants", "boolean flags must have exactly two variants")
	}
	for i, tag := range f.Tags {
		if strings.TrimSpace(tag) == "" {
			p.add(fmt.Sprintf("tags[%d]", i), "must not be blank")
		}
	}
	return p.err()
}

func checkVariantValue(kind FlagKind, raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "is required"
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "is not valid JSON"
	}
	switch kind {
	case KindBoolean:
		if _, ok := v.(bool); !ok {
			return "must be a boolean"
		}
	case KindString:
		if _, ok := v.(string); !ok {
			return "must be a string"
		}
	case KindNumber:
		if _, ok := v.(float64); !ok {
			return "must be a number"
		}
	}
	return ""
}

func canonicalJSON(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	out, _ := json.Marshal(v) // map keys are sorted by encoding/json
	return string(out)
}

// Validate checks the configuration against the flag it configures.
// Segment references are not resolved here because segments live in a
// separate table; the service layer checks them.
func (c *FlagConfig) Validate(f *Flag) error {
	var p problems
	if c.FlagKey != f.Key {
		p.add("flagKey", "does not match flag %q", f.Key)
	}
	variant := func(field, key string) {
		if key == "" {
			p.add(field, "is required")
			return
		}
		if _, ok := f.Variant(key); !ok {
			p.add(field, "unknown variant %q", key)
		}
	}
	variant("offVariant", c.OffVariant)
	validateServe(&p, "fallthrough", c.Fallthrough, variant)

	targeted := map[string]int{}
	for i, t := range c.Targets {
		field := fmt.Sprintf("targets[%d]", i)
		variant(field+".variant", t.Variant)
		if len(t.Values) == 0 {
			p.add(field+".values", "at least one context key is required")
		}
		for _, v := range t.Values {
			if prev, dup := targeted[v]; dup && prev != i {
				p.add(field+".values", "context %q is already targeted by targets[%d]", v, prev)
			}
			targeted[v] = i
		}
	}

	ruleIDs := map[string]bool{}
	for i, r := range c.Rules {
		field := fmt.Sprintf("rules[%d]", i)
		if r.ID == "" {
			p.add(field+".id", "is required")
		} else if ruleIDs[r.ID] {
			p.add(field+".id", "duplicate rule id %q", r.ID)
		}
		ruleIDs[r.ID] = true
		if len(r.Clauses) == 0 {
			p.add(field+".clauses", "at least one clause is required")
		}
		for j, cl := range r.Clauses {
			validateClause(&p, fmt.Sprintf("%s.clauses[%d]", field, j), cl)
		}
		validateServe(&p, field+".serve", r.Serve, variant)
	}

	prereqs := map[string]bool{}
	for i, pr := range c.Prerequisites {
		field := fmt.Sprintf("prerequisites[%d]", i)
		switch {
		case pr.Flag == f.Key:
			p.add(field+".flag", "a flag cannot be its own prerequisite")
		case !ValidKey(pr.Flag):
			p.add(field+".flag", "is not a valid flag key")
		case prereqs[pr.Flag]:
			p.add(field+".flag", "duplicate prerequisite %q", pr.Flag)
		}
		prereqs[pr.Flag] = true
		if pr.Variant == "" {
			p.add(field+".variant", "is required")
		}
	}
	return p.err()
}

func validateServe(p *problems, field string, s Serve, variant func(field, key string)) {
	switch {
	case s.Rollout != nil && s.Variant != "":
		p.add(field, "must set exactly one of variant or rollout")
	case s.Rollout == nil:
		variant(field+".variant", s.Variant)
	default:
		r := s.Rollout
		if len(r.Variants) == 0 {
			p.add(field+".rollout.variants", "at least one slice is required")
		}
		sum := 0
		seen := map[string]bool{}
		for i, wv := range r.Variants {
			wf := fmt.Sprintf("%s.rollout.variants[%d]", field, i)
			variant(wf+".variant", wv.Variant)
			if seen[wv.Variant] {
				p.add(wf+".variant", "variant %q appears twice", wv.Variant)
			}
			seen[wv.Variant] = true
			if wv.Weight < 0 || wv.Weight > TotalWeight {
				p.add(wf+".weight", "must be between 0 and %d", TotalWeight)
			}
			sum += wv.Weight
		}
		if len(r.Variants) > 0 && sum != TotalWeight {
			p.add(field+".rollout.variants", "weights sum to %d, want %d", sum, TotalWeight)
		}
	}
}

func validateClause(p *problems, field string, c Clause) {
	if strings.TrimSpace(c.Attribute) == "" && c.Operator.Class() != ClassSegment {
		p.add(field+".attribute", "is required")
	}
	class := c.Operator.Class()
	if class == ClassUnknown {
		p.add(field+".operator", "unknown operator %q", c.Operator)
		return
	}
	if class != ClassNone && len(c.Values) == 0 {
		p.add(field+".values", "at least one value is required")
	}
	for i, v := range c.Values {
		vf := fmt.Sprintf("%s.values[%d]", field, i)
		switch class {
		case ClassString:
			if _, ok := v.(string); !ok {
				p.add(vf, "must be a string")
			}
		case ClassRegex:
			s, ok := v.(string)
			if !ok {
				p.add(vf, "must be a string")
			} else if _, err := regexp.Compile(s); err != nil {
				p.add(vf, "invalid regular expression: %v", err)
			}
		case ClassNumber:
			if f, ok := v.(float64); !ok || math.IsNaN(f) {
				p.add(vf, "must be a number")
			}
		case ClassTime:
			if _, ok := ParseTimeValue(v); !ok {
				p.add(vf, "must be an RFC 3339 timestamp or unix milliseconds")
			}
		case ClassSemver:
			s, ok := v.(string)
			if !ok {
				p.add(vf, "must be a string")
			} else if _, err := semver.Parse(s); err != nil {
				p.add(vf, "%v", err)
			}
		case ClassSegment:
			if s, ok := v.(string); !ok || !ValidKey(s) {
				p.add(vf, "must be a segment key")
			}
		}
	}
}

// SegmentRefs returns the keys of all segments referenced by clauses in the
// configuration, in first-seen order and without duplicates.
func (c *FlagConfig) SegmentRefs() []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range c.Rules {
		for _, cl := range r.Clauses {
			out = appendSegmentRefs(out, seen, cl)
		}
	}
	return out
}

// SegmentRefs returns the keys of segments referenced by the segment's own
// rules. Segments may nest, and the evaluator guards against cycles.
func (s *Segment) SegmentRefs() []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range s.Rules {
		for _, cl := range r.Clauses {
			out = appendSegmentRefs(out, seen, cl)
		}
	}
	return out
}

func appendSegmentRefs(out []string, seen map[string]bool, cl Clause) []string {
	if cl.Operator.Class() != ClassSegment {
		return out
	}
	for _, v := range cl.Values {
		if s, ok := v.(string); ok && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// Validate checks the segment for internal consistency.
func (s *Segment) Validate() error {
	var p problems
	if !ValidKey(s.Key) {
		p.add("key", "must be 1-128 characters of letters, digits, '.', '_' or '-'")
	}
	if strings.TrimSpace(s.Name) == "" {
		p.add("name", "is required")
	}
	inc := map[string]bool{}
	for _, k := range s.Included {
		inc[k] = true
	}
	for i, k := range s.Excluded {
		if inc[k] {
			p.add(fmt.Sprintf("excluded[%d]", i), "context %q is both included and excluded", k)
		}
	}
	ids := map[string]bool{}
	for i, r := range s.Rules {
		field := fmt.Sprintf("rules[%d]", i)
		if r.ID == "" {
			p.add(field+".id", "is required")
		} else if ids[r.ID] {
			p.add(field+".id", "duplicate rule id %q", r.ID)
		}
		ids[r.ID] = true
		if len(r.Clauses) == 0 {
			p.add(field+".clauses", "at least one clause is required")
		}
		for j, cl := range r.Clauses {
			validateClause(&p, fmt.Sprintf("%s.clauses[%d]", field, j), cl)
			if cl.Operator.Class() == ClassSegment {
				for _, v := range cl.Values {
					if v == s.Key {
						p.add(fmt.Sprintf("%s.clauses[%d]", field, j), "a segment cannot reference itself")
					}
				}
			}
		}
		if r.Weight != nil && (*r.Weight < 0 || *r.Weight > TotalWeight) {
			p.add(field+".weight", "must be between 0 and %d", TotalWeight)
		}
	}
	return p.err()
}

// Validate checks the environment fields that callers control.
func (e *Environment) Validate() error {
	var p problems
	if !ValidKey(e.Key) {
		p.add("key", "must be 1-128 characters of letters, digits, '.', '_' or '-'")
	}
	if strings.TrimSpace(e.Name) == "" {
		p.add("name", "is required")
	}
	return p.err()
}

// ParseTimeValue interprets a clause or attribute value as an instant. It
// accepts RFC 3339 strings (with or without fractional seconds) and numbers,
// which are read as unix milliseconds.
func ParseTimeValue(v any) (time.Time, bool) {
	switch t := v.(type) {
	case string:
		ts, err := time.Parse(time.RFC3339Nano, t)
		if err != nil {
			return time.Time{}, false
		}
		return ts, true
	case float64:
		if math.IsNaN(t) || math.IsInf(t, 0) {
			return time.Time{}, false
		}
		return time.UnixMilli(int64(t)), true
	case int:
		return time.UnixMilli(int64(t)), true
	case int64:
		return time.UnixMilli(t), true
	case time.Time:
		return t, true
	}
	return time.Time{}, false
}

// Package eval is flagpost's flag evaluation engine. It is pure: given a
// model.Snapshot and a Context it computes which variant a flag serves and
// why, without I/O. The server uses it for its evaluate endpoints and the Go
// SDK uses the very same code for local evaluation, so the two can never
// disagree.
package eval

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/J-X0-cloud/flagpost/model"
)

// Reason explains how a result was reached.
type Reason string

// Evaluation reasons.
const (
	ReasonOff                Reason = "OFF"
	ReasonTargetMatch        Reason = "TARGET_MATCH"
	ReasonRuleMatch          Reason = "RULE_MATCH"
	ReasonFallthrough        Reason = "FALLTHROUGH"
	ReasonPrerequisiteFailed Reason = "PREREQUISITE_FAILED"
	ReasonError              Reason = "ERROR"
)

// ErrorKind classifies evaluation failures. Callers typically serve their
// in-code default when a result carries an error.
type ErrorKind string

// Evaluation error kinds.
const (
	ErrFlagNotFound   ErrorKind = "FLAG_NOT_FOUND"
	ErrMalformedFlag  ErrorKind = "MALFORMED_FLAG"
	ErrFlagCycle      ErrorKind = "PREREQUISITE_CYCLE"
	ErrSegmentCycle   ErrorKind = "SEGMENT_CYCLE"
	ErrDepthExceeded  ErrorKind = "DEPTH_EXCEEDED"
	ErrClientNotReady ErrorKind = "CLIENT_NOT_READY"
	ErrWrongType      ErrorKind = "WRONG_TYPE"
)

// Result is the outcome of evaluating one flag for one context.
type Result struct {
	FlagKey     string          `json:"flagKey"`
	FlagVersion int64           `json:"flagVersion,omitempty"`
	Variant     string          `json:"variant,omitempty"`
	Value       json.RawMessage `json:"value,omitempty"`
	Reason      Reason          `json:"reason"`
	// RuleIndex and RuleID identify the matching rule for RULE_MATCH.
	// RuleIndex is -1 otherwise.
	RuleIndex int    `json:"ruleIndex"`
	RuleID    string `json:"ruleId,omitempty"`
	// PrerequisiteKey names the failing prerequisite for
	// PREREQUISITE_FAILED.
	PrerequisiteKey string `json:"prerequisiteKey,omitempty"`
	// InExperiment is true when the variant came from an experiment
	// rollout, which means an exposure should be recorded.
	InExperiment bool      `json:"inExperiment,omitempty"`
	Error        ErrorKind `json:"error,omitempty"`
	Message      string    `json:"message,omitempty"`
	// Trace lists the decisions the engine made, when requested.
	Trace []string `json:"trace,omitempty"`
}

// Failed reports whether the evaluation ended in an error.
func (r Result) Failed() bool { return r.Reason == ReasonError }

// DefaultMaxDepth bounds prerequisite and nested-segment recursion.
const DefaultMaxDepth = 16

// Evaluator evaluates flags against an immutable snapshot. It precomputes
// lookup indexes for individual targets and segment membership lists, so
// construct one per snapshot revision and reuse it; it is safe for
// concurrent use.
type Evaluator struct {
	snap     *model.Snapshot
	maxDepth int
	targets  map[string]map[string]string // flag -> context key -> variant
	segIndex map[string]*segmentIndex
}

type segmentIndex struct {
	included map[string]struct{}
	excluded map[string]struct{}
}

// New builds an Evaluator over snap. The snapshot must not be modified
// afterwards. A nil snapshot evaluates every flag as not found.
func New(snap *model.Snapshot) *Evaluator {
	if snap == nil {
		snap = model.NewSnapshot("", 0)
	}
	e := &Evaluator{
		snap:     snap,
		maxDepth: DefaultMaxDepth,
		targets:  make(map[string]map[string]string, len(snap.Flags)),
		segIndex: make(map[string]*segmentIndex, len(snap.Segments)),
	}
	for key, fs := range snap.Flags {
		if fs == nil || len(fs.Config.Targets) == 0 {
			continue
		}
		m := map[string]string{}
		for _, t := range fs.Config.Targets {
			for _, v := range t.Values {
				if _, dup := m[v]; !dup { // first target wins, matching list order
					m[v] = t.Variant
				}
			}
		}
		e.targets[key] = m
	}
	for key, s := range snap.Segments {
		if s == nil {
			continue
		}
		idx := &segmentIndex{
			included: make(map[string]struct{}, len(s.Included)),
			excluded: make(map[string]struct{}, len(s.Excluded)),
		}
		for _, k := range s.Included {
			idx.included[k] = struct{}{}
		}
		for _, k := range s.Excluded {
			idx.excluded[k] = struct{}{}
		}
		e.segIndex[key] = idx
	}
	return e
}

// Snapshot returns the snapshot the evaluator was built from.
func (e *Evaluator) Snapshot() *model.Snapshot { return e.snap }

// Evaluate evaluates flagKey for ctx.
func (e *Evaluator) Evaluate(flagKey string, ctx Context) Result {
	r := &run{e: e, ctx: ctx}
	return r.flag(flagKey)
}

// Explain evaluates flagKey for ctx and records a human-readable trace of
// every decision in Result.Trace. It is slower than Evaluate and meant for
// debugging tools.
func (e *Evaluator) Explain(flagKey string, ctx Context) Result {
	r := &run{e: e, ctx: ctx, trace: &[]string{}}
	res := r.flag(flagKey)
	res.Trace = *r.trace
	return res
}

// EvaluateAll evaluates every flag in the snapshot for ctx, ordered by key.
func (e *Evaluator) EvaluateAll(ctx Context) []Result {
	keys := make([]string, 0, len(e.snap.Flags))
	for k := range e.snap.Flags {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make([]Result, 0, len(keys))
	r := &run{e: e, ctx: ctx}
	for _, k := range keys {
		out = append(out, r.flag(k))
	}
	return out
}

// run carries the per-evaluation state: the recursion stacks used for cycle
// detection and a memo of segment membership, which is valid because the
// context and snapshot are fixed for the duration of the run.
type run struct {
	e        *Evaluator
	ctx      Context
	trace    *[]string
	flags    []string
	segments []string
	segMemo  map[string]bool
}

func (r *run) tracef(format string, args ...any) {
	if r.trace == nil {
		return
	}
	indent := strings.Repeat("  ", max(0, len(r.flags)-1+len(r.segments)))
	*r.trace = append(*r.trace, indent+fmt.Sprintf(format, args...))
}

type evalError struct {
	kind ErrorKind
	msg  string
}

func (e *evalError) Error() string { return string(e.kind) + ": " + e.msg }

func errResult(key string, version int64, kind ErrorKind, msg string) Result {
	return Result{FlagKey: key, FlagVersion: version, Reason: ReasonError, RuleIndex: -1, Error: kind, Message: msg}
}

func (r *run) flag(key string) Result {
	fs := r.e.snap.Flags[key]
	if fs == nil {
		return errResult(key, 0, ErrFlagNotFound, fmt.Sprintf("flag %q does not exist in this environment", key))
	}
	if slices.Contains(r.flags, key) {
		chain := strings.Join(append(slices.Clone(r.flags), key), " -> ")
		return errResult(key, fs.Config.Version, ErrFlagCycle, "prerequisite cycle: "+chain)
	}
	if len(r.flags) >= r.e.maxDepth {
		return errResult(key, fs.Config.Version, ErrDepthExceeded, "prerequisite chain is too deep")
	}
	r.flags = append(r.flags, key)
	defer func() { r.flags = r.flags[:len(r.flags)-1] }()

	cfg := &fs.Config
	r.tracef("flag %q (config version %d)", key, cfg.Version)
	if !cfg.Enabled {
		r.tracef("flag is off: serving off variant %q", cfg.OffVariant)
		return r.result(fs, cfg.OffVariant, Result{Reason: ReasonOff})
	}

	for _, p := range cfg.Prerequisites {
		sub := r.flag(p.Flag)
		if sub.Error == ErrFlagCycle || sub.Error == ErrDepthExceeded || sub.Error == ErrSegmentCycle {
			// Structural problems poison the whole chain rather than
			// silently serving the off variant.
			return errResult(key, cfg.Version, sub.Error, sub.Message)
		}
		if sub.Reason == ReasonOff || sub.Failed() || sub.Variant != p.Variant {
			r.tracef("prerequisite %q served %q (want %q): serving off variant", p.Flag, sub.Variant, p.Variant)
			return r.result(fs, cfg.OffVariant, Result{Reason: ReasonPrerequisiteFailed, PrerequisiteKey: p.Flag})
		}
		r.tracef("prerequisite %q satisfied with %q", p.Flag, sub.Variant)
	}

	if v, ok := r.e.targets[key][r.ctx.Key]; ok && r.ctx.Key != "" {
		r.tracef("context %q is individually targeted: serving %q", r.ctx.Key, v)
		return r.result(fs, v, Result{Reason: ReasonTargetMatch})
	}

	for i := range cfg.Rules {
		rule := &cfg.Rules[i]
		ok, err := r.clauses(rule.Clauses)
		if err != nil {
			return r.fail(key, cfg.Version, err)
		}
		if !ok {
			r.tracef("rule %d (%s) did not match", i, rule.ID)
			continue
		}
		r.tracef("rule %d (%s) matched", i, rule.ID)
		variant, inExp := r.serve(fs, rule.Serve)
		return r.result(fs, variant, Result{Reason: ReasonRuleMatch, RuleIndex: i, RuleID: rule.ID, InExperiment: inExp})
	}

	r.tracef("no rule matched: serving fallthrough")
	variant, inExp := r.serve(fs, cfg.Fallthrough)
	return r.result(fs, variant, Result{Reason: ReasonFallthrough, InExperiment: inExp})
}

func (r *run) fail(key string, version int64, err error) Result {
	var ee *evalError
	if errors.As(err, &ee) {
		return errResult(key, version, ee.kind, ee.msg)
	}
	return errResult(key, version, ErrMalformedFlag, err.Error())
}

// result fills in the variant value and the fields common to every
// successful evaluation.
func (r *run) result(fs *model.FlagState, variant string, res Result) Result {
	res.FlagKey = fs.Flag.Key
	res.FlagVersion = fs.Config.Version
	if res.Reason != ReasonRuleMatch {
		res.RuleIndex = -1
	}
	v, ok := fs.Flag.Variant(variant)
	if !ok {
		return errResult(fs.Flag.Key, fs.Config.Version, ErrMalformedFlag,
			fmt.Sprintf("configuration refers to unknown variant %q", variant))
	}
	res.Variant = v.Key
	res.Value = v.Value
	return res
}

// serve resolves a Serve to a variant key. For rollouts it hashes the
// configured attribute; a context without a usable value for that attribute
// is placed in bucket 0 and is never counted as part of an experiment, so
// it cannot skew results.
func (r *run) serve(fs *model.FlagState, s model.Serve) (string, bool) {
	if s.Rollout == nil {
		return s.Variant, false
	}
	attr := s.Rollout.BucketBy
	if attr == "" {
		attr = "key"
	}
	seed := fs.Config.Salt
	if seed == "" {
		seed = fs.Flag.Key
	}
	bucket, usable := 0, false
	if raw, ok := r.ctx.Get(attr); ok {
		if bv, ok := bucketValue(raw); ok {
			bucket, usable = Bucket(seed, bv), true
		}
	}
	variant, _ := pickVariant(s.Rollout, bucket)
	r.tracef("rollout by %q: bucket %d of %d -> %q", attr, bucket, model.TotalWeight, variant)
	return variant, usable && s.Rollout.Experiment
}

// clauses reports whether every clause matches (logical AND).
func (r *run) clauses(cs []model.Clause) (bool, error) {
	for i := range cs {
		ok, err := r.clause(&cs[i])
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// clause evaluates one clause. Values are OR-ed: the clause matches when
// any clause value matches. If the attribute is an array, the clause
// matches when any element does. A missing attribute never matches, even
// when the clause is negated; only "exists" observes absence.
func (r *run) clause(c *model.Clause) (bool, error) {
	switch c.Operator {
	case model.OpInSegment, model.OpNotInSegment:
		in := false
		for _, v := range c.Values {
			key, ok := v.(string)
			if !ok {
				continue
			}
			m, err := r.segment(key)
			if err != nil {
				return false, err
			}
			if m {
				in = true
				break
			}
		}
		if c.Operator == model.OpNotInSegment {
			in = !in
		}
		return in != c.Negate, nil
	case model.OpExists:
		v, ok := r.ctx.Get(c.Attribute)
		return (ok && v != nil) != c.Negate, nil
	}

	attr, ok := r.ctx.Get(c.Attribute)
	if !ok || attr == nil {
		return false, nil
	}
	matched := false
	if list, isList := attr.([]any); isList {
		for _, item := range list {
			if r.anyValue(c, item) {
				matched = true
				break
			}
		}
	} else {
		matched = r.anyValue(c, attr)
	}
	if c.Operator == model.OpNotEquals {
		// neq means "equals none of the values".
		matched = !matched
	}
	return matched != c.Negate, nil
}

func (r *run) anyValue(c *model.Clause, attr any) bool {
	for _, want := range c.Values {
		if matchValue(c.Operator, attr, want) {
			return true
		}
	}
	return false
}

// segment reports whether the context belongs to segment key. Explicit
// exclusion beats explicit inclusion, which beats rules. Unknown segments
// contain nobody.
func (r *run) segment(key string) (bool, error) {
	if m, ok := r.segMemo[key]; ok {
		return m, nil
	}
	if slices.Contains(r.segments, key) {
		chain := strings.Join(append(slices.Clone(r.segments), key), " -> ")
		return false, &evalError{kind: ErrSegmentCycle, msg: "segment cycle: " + chain}
	}
	if len(r.segments) >= r.e.maxDepth {
		return false, &evalError{kind: ErrDepthExceeded, msg: "segment nesting is too deep"}
	}
	seg := r.e.snap.Segments[key]
	if seg == nil {
		r.tracef("segment %q does not exist", key)
		return false, nil
	}
	r.segments = append(r.segments, key)
	m, err := r.segmentMembership(seg)
	r.segments = r.segments[:len(r.segments)-1]
	if err != nil {
		return false, err
	}
	if r.segMemo == nil {
		r.segMemo = map[string]bool{}
	}
	r.segMemo[key] = m
	return m, nil
}

func (r *run) segmentMembership(seg *model.Segment) (bool, error) {
	idx := r.e.segIndex[seg.Key]
	if idx != nil && r.ctx.Key != "" {
		if _, ok := idx.excluded[r.ctx.Key]; ok {
			r.tracef("segment %q: context explicitly excluded", seg.Key)
			return false, nil
		}
		if _, ok := idx.included[r.ctx.Key]; ok {
			r.tracef("segment %q: context explicitly included", seg.Key)
			return true, nil
		}
	}
	for i := range seg.Rules {
		rule := &seg.Rules[i]
		ok, err := r.clauses(rule.Clauses)
		if err != nil {
			return false, err
		}
		if !ok {
			continue
		}
		if rule.Weight == nil {
			r.tracef("segment %q: rule %q matched", seg.Key, rule.ID)
			return true, nil
		}
		attr := rule.BucketBy
		if attr == "" {
			attr = "key"
		}
		raw, ok := r.ctx.Get(attr)
		if !ok {
			continue
		}
		bv, ok := bucketValue(raw)
		if !ok {
			continue
		}
		b := Bucket(seg.Key+"."+rule.ID, bv)
		if b < *rule.Weight {
			r.tracef("segment %q: rule %q matched (bucket %d < %d)", seg.Key, rule.ID, b, *rule.Weight)
			return true, nil
		}
	}
	r.tracef("segment %q: no match", seg.Key)
	return false, nil
}

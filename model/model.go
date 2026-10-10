// Package model defines the domain types shared by the flagpost server, the
// evaluation engine and the Go SDK: flags and their variants, per-environment
// flag configuration (targeting rules, rollouts, prerequisites), segments and
// the snapshot that SDKs evaluate against.
//
// All types round-trip through JSON; the JSON form is the wire format of the
// REST API and of the SDK snapshot and stream endpoints.
package model

import (
	"encoding/json"
	"time"
)

// FlagKind is the type of value a flag's variants carry.
type FlagKind string

// Supported flag kinds.
const (
	KindBoolean FlagKind = "boolean"
	KindString  FlagKind = "string"
	KindNumber  FlagKind = "number"
	KindJSON    FlagKind = "json"
)

// Valid reports whether k is a known kind.
func (k FlagKind) Valid() bool {
	switch k {
	case KindBoolean, KindString, KindNumber, KindJSON:
		return true
	}
	return false
}

// TotalWeight is the sum that the weights of a rollout must add up to.
// Weights are expressed in thousandths of a percent, so 100000 is 100% and a
// weight of 1 is 0.001%.
const TotalWeight = 100000

// Environment is an isolated namespace for flag configuration and segments,
// such as "production" or "staging". Each environment has its own SDK key.
type Environment struct {
	Key         string    `json:"key"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	SDKKey      string    `json:"sdkKey,omitempty"`
	Revision    int64     `json:"revision"`
	CreatedAt   time.Time `json:"createdAt"`
}

// Variant is one of the values a flag can serve.
type Variant struct {
	Key         string          `json:"key"`
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Value       json.RawMessage `json:"value"`
}

// Flag is the environment-independent definition of a feature flag: its key,
// kind and the set of variants it may serve.
type Flag struct {
	Key         string    `json:"key"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Kind        FlagKind  `json:"kind"`
	Variants    []Variant `json:"variants"`
	Tags        []string  `json:"tags,omitempty"`
	Archived    bool      `json:"archived,omitempty"`
	Version     int64     `json:"version"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// Variant returns the variant with the given key.
func (f *Flag) Variant(key string) (Variant, bool) {
	for _, v := range f.Variants {
		if v.Key == key {
			return v, true
		}
	}
	return Variant{}, false
}

// FlagConfig is the per-environment configuration of a flag. Evaluation walks
// it in a fixed order: the kill switch, prerequisites, individual targets,
// rules in order, and finally the fallthrough.
type FlagConfig struct {
	FlagKey       string         `json:"flagKey"`
	Environment   string         `json:"environment"`
	Enabled       bool           `json:"enabled"`
	OffVariant    string         `json:"offVariant"`
	Fallthrough   Serve          `json:"fallthrough"`
	Targets       []Target       `json:"targets,omitempty"`
	Rules         []Rule         `json:"rules,omitempty"`
	Prerequisites []Prerequisite `json:"prerequisites,omitempty"`
	// Salt seeds the bucketing hash. Changing it reshuffles every user's
	// bucket for this flag, which is occasionally what you want when
	// restarting an experiment.
	Salt      string    `json:"salt"`
	Version   int64     `json:"version"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Serve describes what to serve once evaluation reaches a rule or the
// fallthrough: either a fixed variant or a weighted rollout.
type Serve struct {
	Variant string   `json:"variant,omitempty"`
	Rollout *Rollout `json:"rollout,omitempty"`
}

// Rollout splits traffic between variants using consistent hashing of a
// context attribute, so the same context always lands in the same variant.
type Rollout struct {
	// BucketBy names the context attribute to hash. Empty means the
	// context key.
	BucketBy string `json:"bucketBy,omitempty"`
	// Experiment marks the rollout as an experiment, which makes SDKs
	// record exposures for it.
	Experiment bool              `json:"experiment,omitempty"`
	Variants   []WeightedVariant `json:"variants"`
}

// WeightedVariant is a rollout slice. Weight is in thousandths of a percent.
type WeightedVariant struct {
	Variant string `json:"variant"`
	Weight  int    `json:"weight"`
}

// Target serves a variant to an explicit list of context keys.
type Target struct {
	Variant string   `json:"variant"`
	Values  []string `json:"values"`
}

// Rule serves a variant or rollout to contexts that satisfy all clauses.
type Rule struct {
	ID          string   `json:"id"`
	Description string   `json:"description,omitempty"`
	Clauses     []Clause `json:"clauses"`
	Serve       Serve    `json:"serve"`
}

// Clause is a single predicate over a context attribute. A clause matches
// when the attribute matches any of Values under Operator; Negate inverts the
// outcome.
type Clause struct {
	Attribute string   `json:"attribute"`
	Operator  Operator `json:"operator"`
	Values    []any    `json:"values"`
	Negate    bool     `json:"negate,omitempty"`
}

// Prerequisite requires another flag to evaluate to a specific variant
// before this flag's targeting is considered.
type Prerequisite struct {
	Flag    string `json:"flag"`
	Variant string `json:"variant"`
}

// Segment is a reusable, per-environment group of contexts that rules can
// reference with the in_segment operator.
type Segment struct {
	Key         string        `json:"key"`
	Environment string        `json:"environment"`
	Name        string        `json:"name"`
	Description string        `json:"description,omitempty"`
	Included    []string      `json:"included,omitempty"`
	Excluded    []string      `json:"excluded,omitempty"`
	Rules       []SegmentRule `json:"rules,omitempty"`
	Version     int64         `json:"version"`
	CreatedAt   time.Time     `json:"createdAt"`
	UpdatedAt   time.Time     `json:"updatedAt"`
}

// SegmentRule includes contexts that satisfy all of its clauses. When Weight
// is set, only that fraction (in thousandths of a percent) of matching
// contexts are included, chosen by consistent hashing.
type SegmentRule struct {
	ID       string   `json:"id"`
	Clauses  []Clause `json:"clauses"`
	Weight   *int     `json:"weight,omitempty"`
	BucketBy string   `json:"bucketBy,omitempty"`
}

// FlagState pairs a flag definition with its configuration in one
// environment. It is the unit that SDKs evaluate.
type FlagState struct {
	Flag   Flag       `json:"flag"`
	Config FlagConfig `json:"config"`
}

// Snapshot is the complete evaluation state of one environment.
type Snapshot struct {
	Environment string                `json:"environment"`
	Revision    int64                 `json:"revision"`
	Flags       map[string]*FlagState `json:"flags"`
	Segments    map[string]*Segment   `json:"segments"`
}

// NewSnapshot returns an empty snapshot for env.
func NewSnapshot(env string, revision int64) *Snapshot {
	return &Snapshot{
		Environment: env,
		Revision:    revision,
		Flags:       map[string]*FlagState{},
		Segments:    map[string]*Segment{},
	}
}

// DefaultConfig returns the configuration a flag receives in an environment
// before anyone has configured it: disabled, serving its last variant when
// off and its first variant on fallthrough. For boolean flags created with
// the conventional [true, false] ordering this means "off serves false".
func DefaultConfig(f *Flag, env string) FlagConfig {
	cfg := FlagConfig{FlagKey: f.Key, Environment: env, Salt: f.Key}
	if n := len(f.Variants); n > 0 {
		cfg.OffVariant = f.Variants[n-1].Key
		cfg.Fallthrough = Serve{Variant: f.Variants[0].Key}
	}
	return cfg
}

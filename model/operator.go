package model

// Operator names a clause comparison.
type Operator string

// Supported clause operators.
const (
	OpEquals       Operator = "eq"
	OpNotEquals    Operator = "neq"
	OpIn           Operator = "in"
	OpContains     Operator = "contains"
	OpStartsWith   Operator = "starts_with"
	OpEndsWith     Operator = "ends_with"
	OpMatches      Operator = "matches"
	OpGreater      Operator = "gt"
	OpGreaterEq    Operator = "gte"
	OpLess         Operator = "lt"
	OpLessEq       Operator = "lte"
	OpBefore       Operator = "before"
	OpAfter        Operator = "after"
	OpSemverEq     Operator = "semver_eq"
	OpSemverGt     Operator = "semver_gt"
	OpSemverLt     Operator = "semver_lt"
	OpExists       Operator = "exists"
	OpInSegment    Operator = "in_segment"
	OpNotInSegment Operator = "not_in_segment"
)

// OperatorClass groups operators by the kind of clause value they expect.
type OperatorClass int

// Operator classes.
const (
	ClassUnknown OperatorClass = iota
	ClassAny                   // compared by JSON equality
	ClassString                // clause values must be strings
	ClassRegex                 // clause values must be valid regular expressions
	ClassNumber                // clause values must be numbers
	ClassTime                  // clause values must be RFC 3339 strings or unix milliseconds
	ClassSemver                // clause values must be semantic versions
	ClassSegment               // clause values must be segment keys
	ClassNone                  // clause values are ignored
)

var operatorClasses = map[Operator]OperatorClass{
	OpEquals:       ClassAny,
	OpNotEquals:    ClassAny,
	OpIn:           ClassAny,
	OpContains:     ClassString,
	OpStartsWith:   ClassString,
	OpEndsWith:     ClassString,
	OpMatches:      ClassRegex,
	OpGreater:      ClassNumber,
	OpGreaterEq:    ClassNumber,
	OpLess:         ClassNumber,
	OpLessEq:       ClassNumber,
	OpBefore:       ClassTime,
	OpAfter:        ClassTime,
	OpSemverEq:     ClassSemver,
	OpSemverGt:     ClassSemver,
	OpSemverLt:     ClassSemver,
	OpExists:       ClassNone,
	OpInSegment:    ClassSegment,
	OpNotInSegment: ClassSegment,
}

// Class returns the operator's class, or ClassUnknown.
func (o Operator) Class() OperatorClass { return operatorClasses[o] }

// Valid reports whether o is a supported operator.
func (o Operator) Valid() bool { return o.Class() != ClassUnknown }

// Operators lists every supported operator in a stable order, for
// documentation and UI pickers.
func Operators() []Operator {
	return []Operator{
		OpEquals, OpNotEquals, OpIn, OpContains, OpStartsWith, OpEndsWith,
		OpMatches, OpGreater, OpGreaterEq, OpLess, OpLessEq, OpBefore, OpAfter,
		OpSemverEq, OpSemverGt, OpSemverLt, OpExists, OpInSegment, OpNotInSegment,
	}
}

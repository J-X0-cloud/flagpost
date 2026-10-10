// Package semver implements parsing and precedence comparison of semantic
// version strings as described by Semantic Versioning 2.0.0.
//
// The parser is deliberately lenient in two ways that matter for targeting
// rules written by humans: a leading "v" is accepted, and missing minor or
// patch components default to zero ("2" parses as 2.0.0, "1.4" as 1.4.0).
// Everything else follows the specification, including the precedence rules
// for pre-release identifiers and the fact that build metadata is ignored.
package semver

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Version is a parsed semantic version.
type Version struct {
	Major, Minor, Patch uint64
	// Pre holds the dot-separated pre-release identifiers, if any.
	Pre []Identifier
	// Build holds the build metadata, which never affects precedence.
	Build string
}

// Identifier is a single pre-release identifier. Numeric identifiers compare
// numerically and always have lower precedence than alphanumeric ones.
type Identifier struct {
	Str     string
	Num     uint64
	Numeric bool
}

// ErrInvalid is wrapped by every parse failure.
var ErrInvalid = errors.New("invalid semantic version")

// Parse parses s into a Version.
func Parse(s string) (Version, error) {
	var v Version
	orig := s
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if s == "" {
		return v, fmt.Errorf("%w: %q is empty", ErrInvalid, orig)
	}

	if i := strings.IndexByte(s, '+'); i >= 0 {
		v.Build = s[i+1:]
		s = s[:i]
		if v.Build == "" || !validIdentList(v.Build, false) {
			return Version{}, fmt.Errorf("%w: %q has malformed build metadata", ErrInvalid, orig)
		}
	}
	if i := strings.IndexByte(s, '-'); i >= 0 {
		pre := s[i+1:]
		s = s[:i]
		if pre == "" || !validIdentList(pre, true) {
			return Version{}, fmt.Errorf("%w: %q has malformed pre-release", ErrInvalid, orig)
		}
		for _, part := range strings.Split(pre, ".") {
			v.Pre = append(v.Pre, newIdentifier(part))
		}
	}

	parts := strings.Split(s, ".")
	if len(parts) > 3 {
		return Version{}, fmt.Errorf("%w: %q has too many components", ErrInvalid, orig)
	}
	nums := [3]uint64{}
	for i, p := range parts {
		n, err := parseNumeric(p)
		if err != nil {
			return Version{}, fmt.Errorf("%w: %q: %v", ErrInvalid, orig, err)
		}
		nums[i] = n
	}
	v.Major, v.Minor, v.Patch = nums[0], nums[1], nums[2]
	return v, nil
}

// MustParse is like Parse but panics on error. It is intended for tests and
// package-level variables.
func MustParse(s string) Version {
	v, err := Parse(s)
	if err != nil {
		panic(err)
	}
	return v
}

func parseNumeric(p string) (uint64, error) {
	if p == "" {
		return 0, errors.New("empty numeric component")
	}
	if len(p) > 1 && p[0] == '0' {
		return 0, fmt.Errorf("numeric component %q has a leading zero", p)
	}
	for i := 0; i < len(p); i++ {
		if p[i] < '0' || p[i] > '9' {
			return 0, fmt.Errorf("numeric component %q contains a non-digit", p)
		}
	}
	n, err := strconv.ParseUint(p, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("numeric component %q overflows", p)
	}
	return n, nil
}

func validIdentList(s string, rejectLeadingZero bool) bool {
	for _, part := range strings.Split(s, ".") {
		if part == "" {
			return false
		}
		allDigits := true
		for i := 0; i < len(part); i++ {
			c := part[i]
			switch {
			case c >= '0' && c <= '9':
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '-':
				allDigits = false
			default:
				return false
			}
		}
		if rejectLeadingZero && allDigits && len(part) > 1 && part[0] == '0' {
			return false
		}
	}
	return true
}

func newIdentifier(s string) Identifier {
	if n, err := strconv.ParseUint(s, 10, 64); err == nil {
		return Identifier{Str: s, Num: n, Numeric: true}
	}
	return Identifier{Str: s}
}

// Compare returns -1, 0 or +1 depending on whether v has lower, equal or
// higher precedence than o.
func (v Version) Compare(o Version) int {
	if c := cmpUint(v.Major, o.Major); c != 0 {
		return c
	}
	if c := cmpUint(v.Minor, o.Minor); c != 0 {
		return c
	}
	if c := cmpUint(v.Patch, o.Patch); c != 0 {
		return c
	}
	// A version without pre-release identifiers has higher precedence.
	switch {
	case len(v.Pre) == 0 && len(o.Pre) == 0:
		return 0
	case len(v.Pre) == 0:
		return 1
	case len(o.Pre) == 0:
		return -1
	}
	for i := 0; i < len(v.Pre) && i < len(o.Pre); i++ {
		if c := v.Pre[i].compare(o.Pre[i]); c != 0 {
			return c
		}
	}
	return cmpInt(len(v.Pre), len(o.Pre))
}

func (a Identifier) compare(b Identifier) int {
	switch {
	case a.Numeric && b.Numeric:
		return cmpUint(a.Num, b.Num)
	case a.Numeric:
		return -1
	case b.Numeric:
		return 1
	default:
		return strings.Compare(a.Str, b.Str)
	}
}

// String formats v in canonical form.
func (v Version) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d.%d.%d", v.Major, v.Minor, v.Patch)
	if len(v.Pre) > 0 {
		b.WriteByte('-')
		for i, id := range v.Pre {
			if i > 0 {
				b.WriteByte('.')
			}
			b.WriteString(id.Str)
		}
	}
	if v.Build != "" {
		b.WriteByte('+')
		b.WriteString(v.Build)
	}
	return b.String()
}

func cmpUint(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

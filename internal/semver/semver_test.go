package semver

import (
	"errors"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"1.2.3", "1.2.3"},
		{"v1.2.3", "1.2.3"},
		{"2", "2.0.0"},
		{"1.4", "1.4.0"},
		{" 1.0.0 ", "1.0.0"},
		{"1.0.0-alpha", "1.0.0-alpha"},
		{"1.0.0-alpha.1", "1.0.0-alpha.1"},
		{"1.0.0-0.3.7", "1.0.0-0.3.7"},
		{"1.0.0-x.7.z.92", "1.0.0-x.7.z.92"},
		{"1.0.0+20130313144700", "1.0.0+20130313144700"},
		{"1.0.0-beta+exp.sha.5114f85", "1.0.0-beta+exp.sha.5114f85"},
		{"1.0.0+001", "1.0.0+001"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			v, err := Parse(tt.in)
			if err != nil {
				t.Fatalf("Parse(%q) error: %v", tt.in, err)
			}
			if got := v.String(); got != tt.want {
				t.Errorf("Parse(%q).String() = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseInvalid(t *testing.T) {
	for _, in := range []string{
		"", "v", "1.2.3.4", "01.2.3", "1.02.3", "1.2.-3", "a.b.c",
		"1.2.3-", "1.2.3+", "1.2.3-01", "1.2.3-a..b", "1.2.3-a$b",
		"1..3", "99999999999999999999.0.0",
	} {
		t.Run(in, func(t *testing.T) {
			_, err := Parse(in)
			if err == nil {
				t.Fatalf("Parse(%q) succeeded, want error", in)
			}
			if !errors.Is(err, ErrInvalid) {
				t.Errorf("Parse(%q) error %v does not wrap ErrInvalid", in, err)
			}
		})
	}
}

func TestComparePrecedenceChain(t *testing.T) {
	// The ordering example from section 11 of the specification.
	chain := []string{
		"1.0.0-alpha",
		"1.0.0-alpha.1",
		"1.0.0-alpha.beta",
		"1.0.0-beta",
		"1.0.0-beta.2",
		"1.0.0-beta.11",
		"1.0.0-rc.1",
		"1.0.0",
		"1.0.1",
		"1.1.0",
		"2.0.0",
	}
	for i := range chain {
		for j := range chain {
			a, b := MustParse(chain[i]), MustParse(chain[j])
			want := cmpInt(i, j)
			if got := a.Compare(b); got != want {
				t.Errorf("Compare(%s, %s) = %d, want %d", chain[i], chain[j], got, want)
			}
		}
	}
}

func TestCompareIgnoresBuild(t *testing.T) {
	a := MustParse("1.0.0+build.1")
	b := MustParse("1.0.0+build.2")
	if c := a.Compare(b); c != 0 {
		t.Errorf("build metadata affected precedence: %d", c)
	}
}

func TestMustParsePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("MustParse did not panic on invalid input")
		}
	}()
	MustParse("not-a-version")
}

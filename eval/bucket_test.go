package eval

import (
	"fmt"
	"math"
	"testing"

	"github.com/J-X0-cloud/flagpost/model"
)

func TestBucketIsDeterministicAndInRange(t *testing.T) {
	for i := 0; i < 1000; i++ {
		v := fmt.Sprintf("user-%d", i)
		a, b := Bucket("seed", v), Bucket("seed", v)
		if a != b {
			t.Fatalf("Bucket not deterministic for %s: %d vs %d", v, a, b)
		}
		if a < 0 || a >= model.TotalWeight {
			t.Fatalf("Bucket(%s) = %d out of range", v, a)
		}
	}
}

// TestBucketGolden pins a few values so an accidental change to the hash
// construction, which would silently move every user between variants, is
// caught by CI.
func TestBucketGolden(t *testing.T) {
	tests := []struct {
		seed, value string
		want        int
	}{
		{"new-checkout", "user-1", 90116},
		{"new-checkout", "user-2", 95626},
		{"pricing-page", "user-1", 82664},
	}
	for _, tt := range tests {
		if got := Bucket(tt.seed, tt.value); got != tt.want {
			t.Errorf("Bucket(%q, %q) = %d, want %d", tt.seed, tt.value, got, tt.want)
		}
	}
}

func TestBucketSeparatorPreventsAmbiguity(t *testing.T) {
	if Bucket("ab", "c") == Bucket("a", "bc") {
		t.Error("seed/value boundary is ambiguous")
	}
}

// TestBucketUniformity runs a chi-square goodness-of-fit test over 20 equal
// bins. With 19 degrees of freedom the 99.9th percentile is 43.82, so a
// correct hash fails this test about once in a thousand seeds; the seed is
// fixed, so the test is deterministic.
func TestBucketUniformity(t *testing.T) {
	const n, bins = 200000, 20
	counts := make([]int, bins)
	for i := 0; i < n; i++ {
		b := Bucket("uniformity", fmt.Sprintf("ctx-%d", i))
		counts[b*bins/model.TotalWeight]++
	}
	expected := float64(n) / bins
	chi2 := 0.0
	for _, c := range counts {
		d := float64(c) - expected
		chi2 += d * d / expected
	}
	if chi2 > 43.82 {
		t.Errorf("chi-square = %.2f exceeds critical value; counts %v", chi2, counts)
	}
}

func TestBucketIndependentAcrossSeeds(t *testing.T) {
	// Users in the first 10% of one flag should be spread across the
	// whole range of an unrelated flag, not clustered at the bottom.
	const n = 50000
	inA, alsoLowB := 0, 0
	for i := 0; i < n; i++ {
		v := fmt.Sprintf("u%d", i)
		if Bucket("flag-a", v) < model.TotalWeight/10 {
			inA++
			if Bucket("flag-b", v) < model.TotalWeight/10 {
				alsoLowB++
			}
		}
	}
	frac := float64(alsoLowB) / float64(inA)
	if math.Abs(frac-0.10) > 0.02 {
		t.Errorf("flag buckets are correlated: %.3f of flag-a's first decile is in flag-b's first decile", frac)
	}
}

func TestPickVariant(t *testing.T) {
	r := &model.Rollout{Variants: []model.WeightedVariant{
		{Variant: "a", Weight: 25000},
		{Variant: "zero", Weight: 0},
		{Variant: "b", Weight: 75000},
	}}
	tests := []struct {
		bucket int
		want   string
	}{
		{0, "a"},
		{24999, "a"},
		{25000, "b"},
		{99999, "b"},
	}
	for _, tt := range tests {
		got, ok := pickVariant(r, tt.bucket)
		if !ok || got != tt.want {
			t.Errorf("pickVariant(%d) = %q, %v; want %q", tt.bucket, got, ok, tt.want)
		}
	}
	short := &model.Rollout{Variants: []model.WeightedVariant{{Variant: "a", Weight: 10}}}
	if got, _ := pickVariant(short, 50000); got != "a" {
		t.Errorf("underweight rollout should fall back to last slice, got %q", got)
	}
	if _, ok := pickVariant(&model.Rollout{}, 0); ok {
		t.Error("empty rollout should not pick a variant")
	}
}

// TestRolloutGrowthIsMonotonic checks the property that makes gradual
// rollouts safe: raising the "on" percentage never turns anyone off.
func TestRolloutGrowthIsMonotonic(t *testing.T) {
	steps := []int{1000, 5000, 10000, 25000, 50000, 100000}
	prevOn := map[string]bool{}
	for _, w := range steps {
		r := &model.Rollout{Variants: []model.WeightedVariant{
			{Variant: "on", Weight: w},
			{Variant: "off", Weight: model.TotalWeight - w},
		}}
		on := map[string]bool{}
		for i := 0; i < 5000; i++ {
			v := fmt.Sprintf("user-%d", i)
			got, _ := pickVariant(r, Bucket("growth", v))
			if got == "on" {
				on[v] = true
			}
		}
		for v := range prevOn {
			if !on[v] {
				t.Fatalf("%s was on at the previous step but off at %d", v, w)
			}
		}
		prevOn = on
	}
	if len(prevOn) != 5000 {
		t.Errorf("at 100%% only %d of 5000 users are on", len(prevOn))
	}
}

func TestBucketValue(t *testing.T) {
	tests := []struct {
		in   any
		want string
		ok   bool
	}{
		{"abc", "abc", true},
		{"", "", false},
		{float64(42), "42", true},
		{float64(-7), "-7", true},
		{4.5, "", false},
		{math.Inf(1), "", false},
		{true, "true", true},
		{nil, "", false},
		{[]any{"a"}, "", false},
	}
	for _, tt := range tests {
		got, ok := bucketValue(tt.in)
		if got != tt.want || ok != tt.ok {
			t.Errorf("bucketValue(%v) = %q, %v; want %q, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func BenchmarkBucket(b *testing.B) {
	for i := 0; i < b.N; i++ {
		Bucket("new-checkout", "user-12345")
	}
}

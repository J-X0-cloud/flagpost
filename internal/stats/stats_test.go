package stats

import (
	"errors"
	"math"
	"testing"
)

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestNormalCDF(t *testing.T) {
	tests := []struct{ z, want float64 }{
		{0, 0.5},
		{1.96, 0.9750021048517796},
		{-1.96, 1 - 0.9750021048517796},
		{8, 1},
	}
	for _, tt := range tests {
		if got := NormalCDF(tt.z); !near(got, tt.want, 1e-12) {
			t.Errorf("NormalCDF(%v) = %v, want %v", tt.z, got, tt.want)
		}
	}
}

func TestNormalQuantile(t *testing.T) {
	tests := []struct{ p, want float64 }{
		{0.5, 0},
		{0.975, 1.9599639845400536},
		{0.001, -3.090232306167813},
		{0.9999, 3.7190164854557084},
	}
	for _, tt := range tests {
		if got := NormalQuantile(tt.p); !near(got, tt.want, 1e-9) {
			t.Errorf("NormalQuantile(%v) = %v, want %v", tt.p, got, tt.want)
		}
	}
	if !math.IsInf(NormalQuantile(0), -1) || !math.IsInf(NormalQuantile(1), 1) {
		t.Error("quantile at the boundaries should be infinite")
	}
	for p := 0.001; p < 1; p += 0.0137 {
		if got := NormalCDF(NormalQuantile(p)); !near(got, p, 1e-12) {
			t.Fatalf("round trip at %v gave %v", p, got)
		}
	}
}

func TestWilson(t *testing.T) {
	tests := []struct {
		s, n     int64
		low, hig float64
	}{
		{81, 263, 0.2552885198782743, 0.3662095769828001},
		{0, 10, 0, 0.27753279986288915},
		{10, 10, 0.7224672001371109, 1},
		{0, 0, 0, 1},
	}
	for _, tt := range tests {
		got := Wilson(tt.s, tt.n, 0.95)
		if !near(got.Low, tt.low, 1e-9) || !near(got.High, tt.hig, 1e-9) {
			t.Errorf("Wilson(%d, %d) = %+v, want [%v, %v]", tt.s, tt.n, got, tt.low, tt.hig)
		}
	}
}

func TestTwoProportionZTest(t *testing.T) {
	got, err := TwoProportionZTest(200, 1000, 250, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if !near(got.Z, 2.677397763008329, 1e-9) || !near(got.PValue, 0.007419649261025674, 1e-9) {
		t.Errorf("got %+v", got)
	}
	rev, _ := TwoProportionZTest(250, 1000, 200, 1000)
	if !near(rev.Z, -got.Z, 1e-12) || !near(rev.PValue, got.PValue, 1e-12) {
		t.Errorf("test is not symmetric: %+v vs %+v", rev, got)
	}
	for _, c := range [][4]int64{{0, 0, 1, 10}, {0, 10, 0, 10}, {10, 10, 5, 5}} {
		if _, err := TwoProportionZTest(c[0], c[1], c[2], c[3]); !errors.Is(err, ErrInsufficientData) {
			t.Errorf("%v: err = %v, want ErrInsufficientData", c, err)
		}
	}
}

// chiSquareSFExact uses closed forms that exist for small degrees of
// freedom, as an independent reference for the incomplete gamma code.
func chiSquareSFExact(x float64, k int) float64 {
	switch {
	case k == 1:
		return math.Erfc(math.Sqrt(x / 2))
	case k == 3:
		return math.Erfc(math.Sqrt(x/2)) + math.Sqrt(2*x/math.Pi)*math.Exp(-x/2)
	case k%2 == 0:
		sum, term := 0.0, 1.0
		for i := 0; i < k/2; i++ {
			if i > 0 {
				term *= (x / 2) / float64(i)
			}
			sum += term
		}
		return math.Exp(-x/2) * sum
	}
	panic("no closed form")
}

func TestChiSquareSF(t *testing.T) {
	for _, k := range []int{1, 2, 3, 4, 10, 20} {
		for _, x := range []float64{0.01, 0.5, 1, 3.84, 7, 15, 30, 80} {
			got, want := ChiSquareSF(x, k), chiSquareSFExact(x, k)
			if !near(got, want, 1e-12) && !near(got/want, 1, 1e-9) {
				t.Errorf("ChiSquareSF(%v, %d) = %v, want %v", x, k, got, want)
			}
		}
	}
	if ChiSquareSF(0, 3) != 1 || ChiSquareSF(-1, 3) != 1 {
		t.Error("sf at or below zero should be 1")
	}
	if !math.IsNaN(ChiSquareSF(1, 0)) || !math.IsNaN(RegularizedGammaQ(-1, 1)) {
		t.Error("invalid parameters should give NaN")
	}
}

func TestSampleRatioMismatch(t *testing.T) {
	tests := []struct {
		name     string
		observed []int64
		weights  []float64
		wantLow  bool
	}{
		{"balanced 50/50", []int64{5012, 4988}, []float64{50, 50}, false},
		{"skewed 50/50", []int64{5500, 4500}, []float64{50, 50}, true},
		{"matches 20/80", []int64{2010, 7990}, []float64{20, 80}, false},
		{"zero weight ignored", []int64{1000, 1000, 400}, []float64{50, 50, 0}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := SampleRatioMismatch(tt.observed, tt.weights)
			if err != nil {
				t.Fatal(err)
			}
			if (p < SRMThreshold) != tt.wantLow {
				t.Errorf("p = %v, want low=%v", p, tt.wantLow)
			}
		})
	}
	if _, err := SampleRatioMismatch([]int64{1}, []float64{1, 2}); err == nil {
		t.Error("length mismatch should error")
	}
	if _, err := SampleRatioMismatch([]int64{1, 1}, []float64{1, -1}); err == nil {
		t.Error("negative weight should error")
	}
	if _, err := SampleRatioMismatch([]int64{0, 0}, []float64{1, 1}); !errors.Is(err, ErrInsufficientData) {
		t.Error("no observations should be insufficient")
	}
	if _, err := SampleRatioMismatch([]int64{5, 5}, []float64{1, 0}); !errors.Is(err, ErrInsufficientData) {
		t.Error("a single weighted arm should be insufficient")
	}
}

func TestAnalyze(t *testing.T) {
	arms := []Arm{
		{Variant: "control", Weight: 50, Exposed: 1000, Converted: 200},
		{Variant: "treatment", Weight: 50, Exposed: 1000, Converted: 250},
	}
	a := Analyze(arms, "control", 0.95)
	if len(a.Arms) != 2 || !a.Arms[0].Control {
		t.Fatalf("unexpected arms %+v", a.Arms)
	}
	tr := a.Arms[1]
	if tr.Lift == nil || !near(*tr.Lift, 0.25, 1e-12) {
		t.Errorf("lift = %v, want 0.25", tr.Lift)
	}
	if tr.PValue == nil || !tr.Significant {
		t.Errorf("treatment should be significant: %+v", tr)
	}
	if a.SRMPValue == nil || a.SRMDetected {
		t.Errorf("balanced exposure flagged as SRM: %+v", a)
	}

	// With three arms the Bonferroni threshold halves to 0.025, so a
	// p-value of about 0.03 is no longer significant.
	three := []Arm{
		{Variant: "control", Weight: 1, Exposed: 2000, Converted: 400},
		{Variant: "b", Weight: 1, Exposed: 2000, Converted: 456},
		{Variant: "c", Weight: 1, Exposed: 2000, Converted: 400},
	}
	b := Analyze(three, "control", 0.95).Arms[1]
	if b.PValue == nil || *b.PValue < 0.025 || *b.PValue > 0.05 || b.Significant {
		t.Errorf("Bonferroni correction not applied: %+v", b)
	}

	empty := Analyze([]Arm{{Variant: "control"}, {Variant: "t"}}, "control", 0.95)
	if empty.Arms[1].PValue != nil || empty.SRMPValue != nil || empty.Arms[1].Rate != 0 {
		t.Errorf("empty experiment produced statistics: %+v", empty)
	}
}

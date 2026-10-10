// Package stats implements the frequentist statistics flagpost uses to
// summarise experiments: Wilson score intervals for conversion rates, the
// pooled two-proportion z-test for comparing a treatment against control,
// and a chi-square goodness-of-fit test for sample ratio mismatch.
//
// Everything is computed from first principles with the standard library;
// the special functions are accurate to well beyond the precision that
// matters for reporting.
package stats

import (
	"errors"
	"math"
)

// NormalCDF is the cumulative distribution function of the standard normal
// distribution.
func NormalCDF(z float64) float64 {
	return 0.5 * math.Erfc(-z/math.Sqrt2)
}

// NormalQuantile returns z such that NormalCDF(z) = p, for 0 < p < 1. It
// uses Acklam's rational approximation refined by one step of Halley's
// method, which gives close to full double precision.
func NormalQuantile(p float64) float64 {
	if p <= 0 {
		return math.Inf(-1)
	}
	if p >= 1 {
		return math.Inf(1)
	}
	a := [...]float64{-3.969683028665376e+01, 2.209460984245205e+02, -2.759285104469687e+02, 1.383577518672690e+02, -3.066479806614716e+01, 2.506628277459239e+00}
	b := [...]float64{-5.447609879822406e+01, 1.615858368580409e+02, -1.556989798598866e+02, 6.680131188771972e+01, -1.328068155288572e+01}
	c := [...]float64{-7.784894002430293e-03, -3.223964580411365e-01, -2.400758277161838e+00, -2.549732539343734e+00, 4.374664141464968e+00, 2.938163982698783e+00}
	d := [...]float64{7.784695709041462e-03, 3.224671290700398e-01, 2.445134137142996e+00, 3.754408661907416e+00}
	const plow = 0.02425
	var x float64
	switch {
	case p < plow:
		q := math.Sqrt(-2 * math.Log(p))
		x = (((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) / ((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	case p <= 1-plow:
		q := p - 0.5
		r := q * q
		x = (((((a[0]*r+a[1])*r+a[2])*r+a[3])*r+a[4])*r + a[5]) * q / (((((b[0]*r+b[1])*r+b[2])*r+b[3])*r+b[4])*r + 1)
	default:
		q := math.Sqrt(-2 * math.Log(1-p))
		x = -(((((c[0]*q+c[1])*q+c[2])*q+c[3])*q+c[4])*q + c[5]) / ((((d[0]*q+d[1])*q+d[2])*q+d[3])*q + 1)
	}
	// Halley refinement.
	e := NormalCDF(x) - p
	u := e * math.Sqrt(2*math.Pi) * math.Exp(x*x/2)
	return x - u/(1+x*u/2)
}

// Interval is a closed confidence interval.
type Interval struct {
	Low  float64 `json:"low"`
	High float64 `json:"high"`
}

// Wilson returns the Wilson score interval for a binomial proportion with
// the given number of successes out of n trials at the given confidence
// level (for example 0.95). Unlike the textbook normal approximation it
// behaves well for small n and for proportions near 0 or 1.
func Wilson(successes, n int64, confidence float64) Interval {
	if n <= 0 {
		return Interval{0, 1}
	}
	z := NormalQuantile(1 - (1-confidence)/2)
	nf := float64(n)
	p := float64(successes) / nf
	z2 := z * z
	denom := 1 + z2/nf
	center := (p + z2/(2*nf)) / denom
	half := z * math.Sqrt(p*(1-p)/nf+z2/(4*nf*nf)) / denom
	return Interval{Low: math.Max(0, center-half), High: math.Min(1, center+half)}
}

// ZTest is the result of a two-proportion z-test.
type ZTest struct {
	Z      float64 `json:"z"`
	PValue float64 `json:"pValue"`
}

// ErrInsufficientData is returned when a test has no variance to work with.
var ErrInsufficientData = errors.New("insufficient data")

// TwoProportionZTest compares the success rate of a treatment (x2 of n2)
// with a control (x1 of n1) using the pooled two-sided z-test.
func TwoProportionZTest(x1, n1, x2, n2 int64) (ZTest, error) {
	if n1 <= 0 || n2 <= 0 {
		return ZTest{}, ErrInsufficientData
	}
	p1 := float64(x1) / float64(n1)
	p2 := float64(x2) / float64(n2)
	pooled := float64(x1+x2) / float64(n1+n2)
	se := math.Sqrt(pooled * (1 - pooled) * (1/float64(n1) + 1/float64(n2)))
	if se == 0 {
		return ZTest{}, ErrInsufficientData
	}
	z := (p2 - p1) / se
	return ZTest{Z: z, PValue: 2 * (1 - NormalCDF(math.Abs(z)))}, nil
}

// ChiSquareSF returns the survival function P(X >= x) of a chi-square
// distribution with k degrees of freedom.
func ChiSquareSF(x float64, k int) float64 {
	if k <= 0 {
		return math.NaN()
	}
	if x <= 0 {
		return 1
	}
	return RegularizedGammaQ(float64(k)/2, x/2)
}

// RegularizedGammaQ computes the regularized upper incomplete gamma function
// Q(a, x) = Γ(a, x) / Γ(a). It uses the power series for P when x < a+1 and
// Lentz's continued fraction for Q otherwise, as in Numerical Recipes.
func RegularizedGammaQ(a, x float64) float64 {
	if x < 0 || a <= 0 {
		return math.NaN()
	}
	if x == 0 {
		return 1
	}
	if x < a+1 {
		return 1 - gammaPSeries(a, x)
	}
	return gammaQContinuedFraction(a, x)
}

const (
	gammaEps     = 1e-15
	gammaMaxIter = 500
	tiny         = 1e-300
)

func lgamma(a float64) float64 {
	v, _ := math.Lgamma(a)
	return v
}

func gammaPSeries(a, x float64) float64 {
	sum := 1 / a
	term := sum
	ap := a
	for i := 0; i < gammaMaxIter; i++ {
		ap++
		term *= x / ap
		sum += term
		if math.Abs(term) < math.Abs(sum)*gammaEps {
			break
		}
	}
	return sum * math.Exp(-x+a*math.Log(x)-lgamma(a))
}

func gammaQContinuedFraction(a, x float64) float64 {
	b := x + 1 - a
	c := 1 / tiny
	d := 1 / b
	h := d
	for i := 1; i <= gammaMaxIter; i++ {
		an := -float64(i) * (float64(i) - a)
		b += 2
		d = an*d + b
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = b + an/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		delta := d * c
		h *= delta
		if math.Abs(delta-1) < gammaEps {
			break
		}
	}
	return math.Exp(-x+a*math.Log(x)-lgamma(a)) * h
}

// SampleRatioMismatch tests whether observed exposure counts are consistent
// with the configured traffic weights. A very small p-value (conventionally
// below 0.001) means assignment or logging is broken and the experiment's
// results should not be trusted. Variants with zero expected weight are
// ignored. It returns ErrInsufficientData when fewer than two variants have
// weight or nothing was observed.
func SampleRatioMismatch(observed []int64, weights []float64) (float64, error) {
	if len(observed) != len(weights) {
		return 0, errors.New("observed and weights differ in length")
	}
	var total int64
	var wsum float64
	k := 0
	for i, w := range weights {
		if w < 0 {
			return 0, errors.New("negative weight")
		}
		if w > 0 {
			total += observed[i]
			wsum += w
			k++
		}
	}
	if k < 2 || total == 0 {
		return 0, ErrInsufficientData
	}
	chi2 := 0.0
	for i, w := range weights {
		if w == 0 {
			continue
		}
		exp := float64(total) * w / wsum
		d := float64(observed[i]) - exp
		chi2 += d * d / exp
	}
	return ChiSquareSF(chi2, k-1), nil
}

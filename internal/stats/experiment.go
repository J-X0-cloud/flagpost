package stats

import "math"

// Arm is the raw data for one variant of an experiment.
type Arm struct {
	Variant   string
	Weight    float64 // configured traffic share, any scale
	Exposed   int64   // distinct contexts that saw the variant
	Converted int64   // distinct exposed contexts that converted
}

// ArmResult summarises one variant relative to the control.
type ArmResult struct {
	Variant     string   `json:"variant"`
	Exposed     int64    `json:"exposed"`
	Converted   int64    `json:"converted"`
	Rate        float64  `json:"rate"`
	Interval    Interval `json:"interval"`
	Control     bool     `json:"control,omitempty"`
	Lift        *float64 `json:"lift,omitempty"`
	Z           *float64 `json:"z,omitempty"`
	PValue      *float64 `json:"pValue,omitempty"`
	Significant bool     `json:"significant"`
}

// Analysis is the summary of a whole experiment.
type Analysis struct {
	Confidence float64     `json:"confidence"`
	Arms       []ArmResult `json:"arms"`
	// SRMPValue is the sample ratio mismatch p-value, absent when it
	// cannot be computed.
	SRMPValue *float64 `json:"srmPValue,omitempty"`
	// SRMDetected is true when SRMPValue is below SRMThreshold.
	SRMDetected bool `json:"srmDetected"`
}

// SRMThreshold is the p-value below which a sample ratio mismatch is
// reported. It is deliberately strict because the test runs continuously.
const SRMThreshold = 0.001

// Analyze compares every arm against the arm named control at the given
// confidence level. Each comparison uses a Bonferroni-corrected threshold
// so that adding arms does not inflate the family-wise false positive rate.
func Analyze(arms []Arm, control string, confidence float64) Analysis {
	out := Analysis{Confidence: confidence}
	var ctl *Arm
	for i := range arms {
		if arms[i].Variant == control {
			ctl = &arms[i]
		}
	}
	comparisons := len(arms) - 1
	if comparisons < 1 {
		comparisons = 1
	}
	alpha := (1 - confidence) / float64(comparisons)

	observed := make([]int64, len(arms))
	weights := make([]float64, len(arms))
	for i, a := range arms {
		observed[i], weights[i] = a.Exposed, a.Weight
		res := ArmResult{
			Variant:   a.Variant,
			Exposed:   a.Exposed,
			Converted: a.Converted,
			Interval:  Wilson(a.Converted, a.Exposed, confidence),
			Control:   a.Variant == control,
		}
		if a.Exposed > 0 {
			res.Rate = float64(a.Converted) / float64(a.Exposed)
		}
		if ctl != nil && !res.Control {
			if ctl.Exposed > 0 && ctl.Converted > 0 && a.Exposed > 0 {
				ctlRate := float64(ctl.Converted) / float64(ctl.Exposed)
				lift := (res.Rate - ctlRate) / ctlRate
				res.Lift = &lift
			}
			if zt, err := TwoProportionZTest(ctl.Converted, ctl.Exposed, a.Converted, a.Exposed); err == nil {
				z, p := zt.Z, zt.PValue
				res.Z, res.PValue = &z, &p
				res.Significant = p < alpha
			}
		}
		out.Arms = append(out.Arms, res)
	}
	if p, err := SampleRatioMismatch(observed, weights); err == nil && !math.IsNaN(p) {
		out.SRMPValue = &p
		out.SRMDetected = p < SRMThreshold
	}
	return out
}

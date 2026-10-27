package outcomebench

import "math"

// MinN is the smallest sample a rate or median is reported for; below it the
// figure is "not reported", never a number. It matches the hub's evidence
// panel (pkg/hub minEvidenceN).
const MinN = 10

// Wilson returns the 95% Wilson score interval for k successes in n trials.
// It is the same formula the hub's evidence panel uses (pkg/hub/evidence.go
// wilson); it lives here too so the benchmark does not import the hub.
func Wilson(k, n int) (lo, hi float64) {
	if n <= 0 {
		return 0, 0
	}
	const z = 1.96
	nf, p := float64(n), float64(k)/float64(n)
	d := 1 + z*z/nf
	centre := (p + z*z/(2*nf)) / d
	half := z * math.Sqrt(p*(1-p)/nf+z*z/(4*nf*nf)) / d
	return math.Max(0, centre-half), math.Min(1, centre+half)
}

// Rate is k/n with its Wilson interval. Reported is false when n < MinN; a
// renderer must then print "not reported" and nothing numeric.
type Rate struct {
	K, N     int
	Rate     float64
	Lo, Hi   float64
	Reported bool
}

// NewRate builds a Rate, leaving it unreported below MinN.
func NewRate(k, n int) Rate {
	r := Rate{K: k, N: n}
	if n < MinN {
		return r
	}
	r.Rate = float64(k) / float64(n)
	r.Lo, r.Hi = Wilson(k, n)
	r.Reported = true
	return r
}

// Median is a median over n samples, unreported below MinN.
type Median struct {
	Value    float64
	N        int
	Reported bool
}

// NewMedian sorts a copy of xs and takes the lower median.
func NewMedian(xs []float64) Median {
	m := Median{N: len(xs)}
	if len(xs) < MinN {
		return m
	}
	s := append([]float64(nil), xs...)
	sortFloats(s)
	m.Value = s[len(s)/2]
	m.Reported = true
	return m
}

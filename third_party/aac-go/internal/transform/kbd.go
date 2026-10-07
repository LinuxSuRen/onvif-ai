package transform

import "math"

// KBDWindow returns the Kaiser-Bessel derived window of length n —
// ISO/IEC 14496-3 §4.6.11.3.2 (window_shape == 1). AAC uses alpha = 4 for
// long windows (n = 2048) and alpha = 6 for short windows (n = 256).
//
// The rising half is the square root of the normalized cumulative sum of an
// (n/2+1)-point Kaiser window kernel — the standard's defining form — and
// the falling half mirrors it. With the kernel's symmetry this satisfies
// the Princen-Bradley condition w[i]² + w[i+n/2]² = 1 exactly (verified in
// tests), and the resulting windows are verified against reference
// decoders on KBD-shaped streams in the acceptance suite.
func KBDWindow(n int, alpha float64) []float64 {
	if n <= 0 || n%2 != 0 {
		panic("transform: KBD window length must be positive and even")
	}
	half := n / 2

	// Kaiser kernel over half+1 symmetric points.
	alpha2 := 4 * (alpha * math.Pi / float64(half)) * (alpha * math.Pi / float64(half))
	kernel := make([]float64, half+1)
	total := 0.0
	for i := range kernel {
		kernel[i] = besselI0(math.Sqrt(float64(i) * float64(half-i) * alpha2))
		total += kernel[i]
	}

	w := make([]float64, n)
	sum := 0.0
	for i := 0; i < half; i++ {
		sum += kernel[i]
		w[i] = math.Sqrt(sum / total)
		w[n-1-i] = w[i]
	}
	return w
}

// besselI0 evaluates the zeroth-order modified Bessel function of the first
// kind by its power series Σ ((x/2)^k / k!)², which converges quickly for
// the arguments used in KBD windows.
func besselI0(x float64) float64 {
	sum, term := 1.0, 1.0
	q := x * x / 4
	for k := 1.0; ; k++ {
		term *= q / (k * k)
		sum += term
		if term < sum*1e-21 {
			return sum
		}
	}
}

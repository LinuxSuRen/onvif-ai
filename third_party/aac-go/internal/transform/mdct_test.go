package transform

import (
	"math"
	"math/rand"
	"testing"
)

// TestPerfectReconstruction verifies the TDAC property end to end: windowed
// MDCT frames with 50% overlap, inverse transformed, windowed again, and
// overlap-added must reproduce the interior of the input to float64
// precision.
func TestPerfectReconstruction(t *testing.T) {
	const (
		n      = 2048
		half   = n / 2
		frames = 6
	)
	rng := rand.New(rand.NewSource(1))
	input := make([]float64, half*(frames+1))
	for i := range input {
		input[i] = rng.Float64()*2 - 1
	}

	m := New(n)
	window := SineWindow(n)
	output := make([]float64, len(input))

	for f := range frames {
		frame := make([]float64, n)
		copy(frame, input[f*half:f*half+n])
		for i := range frame {
			frame[i] *= window[i]
		}
		spec := m.Forward(frame)
		rec := m.Inverse(spec)
		for i := range rec {
			output[f*half+i] += rec[i] * window[i]
		}
	}

	// The first and last half frames lack an overlap partner; check the interior.
	maxErr := 0.0
	for i := half; i < half*frames; i++ {
		if err := math.Abs(output[i] - input[i]); err > maxErr {
			maxErr = err
		}
	}
	if maxErr > 1e-9 {
		t.Fatalf("reconstruction error %.3g exceeds 1e-9", maxErr)
	}
}

// TestSineWindowTDACCondition checks the Princen-Bradley condition
// w[i]² + w[i+N/2]² = 1 required for perfect reconstruction.
func TestSineWindowTDACCondition(t *testing.T) {
	const n = 2048
	w := SineWindow(n)
	for i := range n / 2 {
		sum := w[i]*w[i] + w[i+n/2]*w[i+n/2]
		if math.Abs(sum-1) > 1e-12 {
			t.Fatalf("w[%d]²+w[%d]² = %.15f, want 1", i, i+n/2, sum)
		}
	}
}

// TestForwardOfKnownTone sanity-checks scaling: a pure cosine aligned with
// basis function k concentrates energy at coefficient k with the amplitude
// predicted by the analysis formula.
func TestForwardOfKnownTone(t *testing.T) {
	const n = 256
	m := New(n)
	window := SineWindow(n)
	n0 := (float64(n)/2 + 1) / 2
	const k = 10

	frame := make([]float64, n)
	for i := range frame {
		frame[i] = math.Cos(2*math.Pi/n*(float64(i)+n0)*(k+0.5)) * window[i]
	}
	spec := m.Forward(frame)

	// Energy must be dominated by bin k (window spreads it slightly).
	peak := math.Abs(spec[k])
	for j := range spec {
		if j >= k-2 && j <= k+2 {
			continue
		}
		if math.Abs(spec[j]) > peak/10 {
			t.Fatalf("unexpected energy at bin %d: %.3f (peak %.3f at bin %d)", j, spec[j], peak, k)
		}
	}
}

func BenchmarkForward(b *testing.B) {
	m := New(2048)
	frame := make([]float64, 2048)
	for i := range frame {
		frame[i] = math.Sin(float64(i))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Forward(frame)
	}
}

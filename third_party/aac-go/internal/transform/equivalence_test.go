package transform

import (
	"math"
	"math/rand"
	"testing"
)

// TestFastMatchesDirect pins the FFT-based transform to the direct
// evaluation of the ISO/IEC 14496-3 §4.6.11 formulas. The direct
// implementation is the permanent reference; the fast path must be an
// unobservable optimization.
//
// The tolerance is relative to the frame's peak magnitude, not the
// individual coefficient: at strong cancellation points the O(N²) direct
// sum itself carries ~√N·ε·peak of rounding error, so a per-coefficient
// relative gate would fail correct implementations (the FFT is typically
// *more* accurate there). A wrong twiddle or sign produces errors on the
// order of the signal itself, ~9 orders above this gate. Both AAC sizes
// and both directions are exercised over many random frames, including
// int16-scale amplitudes.
func TestFastMatchesDirect(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for _, n := range []int{256, 2048} {
		m := New(n)
		for frame := range 200 {
			scale := 1.0
			if frame%2 == 1 {
				scale = 32768 // int16-scaled input, as the encoder feeds it
			}

			z := make([]float64, n)
			for i := range z {
				z[i] = (rng.Float64()*2 - 1) * scale
			}
			wantF := m.directForward(z)
			gotF := m.Forward(z)
			compare(t, "Forward", n, frame, gotF, wantF)

			spec := make([]float64, n/2)
			for i := range spec {
				spec[i] = (rng.Float64()*2 - 1) * scale * 100
			}
			wantI := m.directInverse(spec)
			gotI := m.Inverse(spec)
			compare(t, "Inverse", n, frame, gotI, wantI)
		}
	}
}

func compare(t *testing.T, dir string, n, frame int, got, want []float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s n=%d frame %d: length %d, want %d", dir, n, frame, len(got), len(want))
	}
	peak := 1.0
	for _, v := range want {
		peak = math.Max(peak, math.Abs(v))
	}
	tolerance := 1e-11 * peak
	for i := range want {
		if diff := math.Abs(got[i] - want[i]); diff > tolerance {
			t.Fatalf("%s n=%d frame %d coefficient %d: fast %.15g, direct %.15g (diff %.3g > tol %.3g)",
				dir, n, frame, i, got[i], want[i], diff, tolerance)
		}
	}
}

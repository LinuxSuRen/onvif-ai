package transform

import (
	"math"
	"testing"
)

// TestKBDWindowTDACCondition checks the Princen-Bradley condition for the
// two KBD windows AAC uses (long n=2048 α=4, short n=256 α=6).
func TestKBDWindowTDACCondition(t *testing.T) {
	for _, tc := range []struct {
		n     int
		alpha float64
	}{{2048, 4}, {256, 6}} {
		w := KBDWindow(tc.n, tc.alpha)
		for i := 0; i < tc.n/2; i++ {
			sum := w[i]*w[i] + w[i+tc.n/2]*w[i+tc.n/2]
			if math.Abs(sum-1) > 1e-12 {
				t.Fatalf("n=%d α=%g: w[%d]²+w[%d]² = %.15f, want 1", tc.n, tc.alpha, i, i+tc.n/2, sum)
			}
		}
	}
}

// TestKBDWindowShape pins basic properties: symmetric, rising on the left
// half, values in (0, 1), and steeper skirts than the sine window (the
// defining property of KBD's better stopband).
func TestKBDWindowShape(t *testing.T) {
	const n = 2048
	w := KBDWindow(n, 4)
	sineW := SineWindow(n)
	for i := 0; i < n/2; i++ {
		if w[i] != w[n-1-i] {
			t.Fatalf("asymmetry at %d", i)
		}
		if i > 0 && w[i] < w[i-1] {
			t.Fatalf("left half not monotonically rising at %d", i)
		}
		if w[i] <= 0 || w[i] > 1 {
			t.Fatalf("w[%d] = %g out of (0, 1]", i, w[i])
		}
	}
	if !(w[0] < sineW[0]) {
		t.Fatalf("KBD edge %.6g not below sine edge %.6g", w[0], sineW[0])
	}

	// Perfect reconstruction through the MDCT with KBD windows.
	m := New(256)
	kbd := KBDWindow(256, 6)
	input := make([]float64, 128*4)
	for i := range input {
		input[i] = math.Sin(float64(i) / 3)
	}
	output := make([]float64, len(input))
	for f := 0; f+256 <= len(input); f += 128 {
		frame := make([]float64, 256)
		copy(frame, input[f:f+256])
		for i := range frame {
			frame[i] *= kbd[i]
		}
		rec := m.Inverse(m.Forward(frame))
		for i := range rec {
			output[f+i] += rec[i] * kbd[i]
		}
	}
	for i := 128; i < 256; i++ {
		if math.Abs(output[i]-input[i]) > 1e-9 {
			t.Fatalf("KBD reconstruction error %.3g at %d", math.Abs(output[i]-input[i]), i)
		}
	}
}

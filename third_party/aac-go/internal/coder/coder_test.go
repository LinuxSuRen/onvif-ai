package coder

import (
	"math"
	"math/rand"
	"testing"

	"github.com/arabian9ts/aac-go/internal/bits"
	"github.com/arabian9ts/aac-go/internal/tables"
)

func TestScalefactorDeltaRoundtrip(t *testing.T) {
	w := bits.NewWriter()
	for delta := -60; delta <= 60; delta++ {
		if err := EncodeScalefactorDelta(w, delta); err != nil {
			t.Fatalf("encode %d: %v", delta, err)
		}
	}
	r := bits.NewReader(w.Bytes())
	for delta := -60; delta <= 60; delta++ {
		got, err := DecodeScalefactorDelta(r)
		if err != nil {
			t.Fatalf("decode %d: %v", delta, err)
		}
		if got != delta {
			t.Fatalf("roundtrip: got %d, want %d", got, delta)
		}
	}
}

// TestSpectralRoundtripAllBooks encodes random in-range tuples with every
// codebook and decodes them back, also checking SpectralBits agrees with
// the bits actually written.
func TestSpectralRoundtripAllBooks(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for book := 1; book <= 11; book++ {
		cb := &tables.SpectrumCodebooks[book-1]
		const tuples = 64
		q := make([]int32, tuples*cb.Dim)
		for i := range q {
			switch {
			case book == 11:
				// Exercise zeros, in-table values, and escapes up to 8191.
				q[i] = int32(rng.Intn(2*8191+1) - 8191)
				if rng.Intn(3) == 0 {
					q[i] = int32(rng.Intn(31) - 15)
				}
			case cb.Signed:
				q[i] = int32(rng.Intn(2*cb.LAV+1) - cb.LAV)
			default:
				q[i] = int32(rng.Intn(2*cb.LAV+1) - cb.LAV)
			}
		}
		if !FitsCodebook(book, q) {
			t.Fatalf("book %d: generated values do not fit", book)
		}

		wantBits, err := SpectralBits(book, q)
		if err != nil {
			t.Fatalf("book %d: SpectralBits: %v", book, err)
		}
		w := bits.NewWriter()
		if err := EncodeSpectral(w, book, q); err != nil {
			t.Fatalf("book %d: encode: %v", book, err)
		}
		if w.Len() != wantBits {
			t.Fatalf("book %d: SpectralBits = %d but wrote %d", book, wantBits, w.Len())
		}

		r := bits.NewReader(w.Bytes())
		got := make([]int32, len(q))
		if err := DecodeSpectral(r, book, got); err != nil {
			t.Fatalf("book %d: decode: %v", book, err)
		}
		for i := range q {
			if got[i] != q[i] {
				t.Fatalf("book %d: value %d: got %d, want %d", book, i, got[i], q[i])
			}
		}
	}
}

func TestDequantizeInvertsQuantization(t *testing.T) {
	// Quantize with the encoder's formula at several scalefactors and
	// check Dequantize lands within the quantization step.
	quantize := func(x float64, sf int) int32 {
		if x == 0 {
			return 0
		}
		mag := math.Pow(math.Abs(x), 0.75) * math.Exp2(-3.0*float64(sf-ScalefactorOffset)/16.0)
		q := int32(mag + 0.4054)
		if x < 0 {
			return -q
		}
		return q
	}
	for _, sf := range []int{80, 100, 120, 148} {
		for _, x := range []float64{0, 1, 100.5, -3333, 1e6, -1.2e7} {
			q := quantize(x, sf)
			back := Dequantize(q, sf)
			// One quantization step at this scalefactor.
			step := math.Exp2(float64(sf-ScalefactorOffset) / 4.0)
			if q != 0 {
				// Relative check: |q|^(4/3) spacing grows with magnitude.
				next := Dequantize(q+1, sf) - back
				if d := math.Abs(x - back); d > next {
					t.Fatalf("sf %d x %g: dequant %g misses by %g (> step %g)", sf, x, back, d, next)
				}
			} else if math.Abs(x) > step*2 {
				t.Fatalf("sf %d x %g quantized to zero unexpectedly", sf, x)
			}
		}
	}
}

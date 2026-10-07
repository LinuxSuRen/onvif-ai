package aac

import (
	"bytes"
	"errors"
	"io"
	"math"
	"math/rand"
	"testing"

	"github.com/arabian9ts/aac-go/internal/bits"
	"github.com/arabian9ts/aac-go/internal/tables"
)

func TestParseTNSLong(t *testing.T) {
	w := bits.NewWriter()
	writeTestBits(t, w, 2, 2)  // n_filt
	writeTestBits(t, w, 1, 1)  // coef_res
	writeTestBits(t, w, 5, 6)  // length
	writeTestBits(t, w, 2, 5)  // order
	writeTestBits(t, w, 1, 1)  // direction
	writeTestBits(t, w, 0, 1)  // coef_compress
	writeTestBits(t, w, 1, 4)  // coef[0]
	writeTestBits(t, w, 15, 4) // coef[1]
	writeTestBits(t, w, 3, 6)  // length
	writeTestBits(t, w, 0, 5)  // order

	r := bits.NewReader(w.Bytes())
	tns, err := parseTNS(r, 0)
	if err != nil {
		t.Fatalf("parseTNS: %v", err)
	}
	if got, want := len(tns.windows), 1; got != want {
		t.Fatalf("windows = %d, want %d", got, want)
	}
	filters := tns.windows[0].filters
	if got, want := len(filters), 2; got != want {
		t.Fatalf("filters = %d, want %d", got, want)
	}
	if filters[0].length != 5 || !filters[0].direction {
		t.Fatalf("filter 0 = %+v, want length 5 and reverse direction", filters[0])
	}
	wantCoefficients := []float64{tables.TNSCoefficients[1][1], tables.TNSCoefficients[1][15]}
	checkFloatSlice(t, filters[0].coefficients, wantCoefficients, 0)
	if filters[1].length != 3 || len(filters[1].coefficients) != 0 {
		t.Fatalf("filter 1 = %+v, want zero-order length-3 filter", filters[1])
	}
	if got, want := r.Position(), 35; got != want {
		t.Fatalf("reader position = %d, want %d", got, want)
	}
}

func TestParseTNSShort(t *testing.T) {
	w := bits.NewWriter()
	writeTestBits(t, w, 1, 1) // window 0 n_filt
	writeTestBits(t, w, 0, 1) // coef_res
	writeTestBits(t, w, 3, 4) // length
	writeTestBits(t, w, 7, 3) // maximum short order
	writeTestBits(t, w, 0, 1) // direction
	writeTestBits(t, w, 1, 1) // coef_compress: 2-bit coefficients
	for code := range 7 {
		writeTestBits(t, w, uint64(code%4), 2)
	}
	for range 7 {
		writeTestBits(t, w, 0, 1) // remaining windows n_filt
	}

	tns, err := parseTNS(bits.NewReader(w.Bytes()), eightShortSequence)
	if err != nil {
		t.Fatalf("parseTNS: %v", err)
	}
	if got, want := len(tns.windows), 8; got != want {
		t.Fatalf("windows = %d, want %d", got, want)
	}
	filter := tns.windows[0].filters[0]
	if filter.length != 3 || filter.direction || len(filter.coefficients) != 7 {
		t.Fatalf("filter = %+v", filter)
	}
	for i, got := range filter.coefficients {
		want := tables.TNSCoefficients[2][i%4]
		if got != want {
			t.Fatalf("coefficient %d = %g, want %g", i, got, want)
		}
	}
	for window := 1; window < 8; window++ {
		if len(tns.windows[window].filters) != 0 {
			t.Fatalf("window %d unexpectedly has filters", window)
		}
	}
}

func TestParseTNSErrors(t *testing.T) {
	t.Run("order exceeds AAC-LC maximum", func(t *testing.T) {
		w := bits.NewWriter()
		writeTestBits(t, w, 1, 2)  // n_filt
		writeTestBits(t, w, 0, 1)  // coef_res
		writeTestBits(t, w, 1, 6)  // length
		writeTestBits(t, w, 13, 5) // order
		if _, err := parseTNS(bits.NewReader(w.Bytes()), 0); err == nil {
			t.Fatal("parseTNS accepted long-window order 13")
		}
	})

	t.Run("truncated", func(t *testing.T) {
		if _, err := parseTNS(bits.NewReader([]byte{0xff}), 0); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("error = %v, want io.ErrUnexpectedEOF", err)
		}
	})
}

func TestApplyTNS(t *testing.T) {
	tests := []struct {
		name         string
		direction    bool
		coefficients []float64
		spec         []float64
		want         []float64
	}{
		{
			name:         "forward order one",
			coefficients: []float64{0.5},
			spec:         []float64{0, 0, 2, 3},
			want:         []float64{0, 0, 2, 4},
		},
		{
			name:         "reverse order one",
			direction:    true,
			coefficients: []float64{0.5},
			spec:         []float64{0, 0, 2, 3},
			want:         []float64{0, 0, 3.5, 3},
		},
		{
			name:         "reflection to LPC order two",
			coefficients: []float64{0.5, 0.25},
			spec:         []float64{0, 1, 2, 3},
			want:         []float64{0, 1, 2.375, 4.140625},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tns := &tnsData{windows: []tnsWindow{{filters: []tnsFilter{{
				length:       1,
				direction:    tc.direction,
				coefficients: tc.coefficients,
			}}}}}
			got := append([]float64(nil), tc.spec...)
			offsets := []uint16{0, 1, 4}
			if len(tc.coefficients) == 1 {
				offsets = []uint16{0, 2, 4}
			}
			applyTNS(got, tns, 0, offsets, 2)
			checkFloatSlice(t, got, tc.want, 1e-15)
		})
	}
}

func TestApplyTNSCoefficientSign(t *testing.T) {
	w := bits.NewWriter()
	writeTestBits(t, w, 1, 2) // n_filt
	writeTestBits(t, w, 0, 1) // coef_res: 3-bit coefficients
	writeTestBits(t, w, 1, 6) // length
	writeTestBits(t, w, 1, 5) // order
	writeTestBits(t, w, 0, 1) // direction
	writeTestBits(t, w, 0, 1) // coef_compress
	writeTestBits(t, w, 1, 3) // ff_tns_tmp2_map[0][1] = -sin(pi/7)

	tns, err := parseTNS(bits.NewReader(w.Bytes()), 0)
	if err != nil {
		t.Fatalf("parseTNS: %v", err)
	}
	spec := []float64{2, 3}
	applyTNS(spec, tns, 0, []uint16{0, 2}, 1)
	want := []float64{2, 3 - 2*math.Sin(math.Pi/7)}
	checkFloatSlice(t, spec, want, 1e-7)
}

func TestApplyTNSShortWindowOffset(t *testing.T) {
	tns := &tnsData{windows: make([]tnsWindow, 8)}
	tns.windows[2].filters = []tnsFilter{{length: 1, coefficients: []float64{0.5}}}
	spec := make([]float64, 8*128)
	spec[2*128+2], spec[2*128+3] = 2, 3
	applyTNS(spec, tns, 2, []uint16{0, 2, 128}, 2)
	if spec[2*128+2] != 2 || spec[2*128+3] != 4 {
		t.Fatalf("window 2 band = %v, want [2 4]", spec[2*128+2:2*128+4])
	}
	if spec[0] != 0 || spec[128] != 0 || spec[3*128] != 0 {
		t.Fatal("applyTNS changed another short window")
	}
}

func TestAnalyzeTNSLongRoundTrip(t *testing.T) {
	offsets := []uint16{0, 256, 512, 768, 1024}
	original := predictiveSpectrum(1024, 0.92, 17)
	encoded := append([]float64(nil), original...)

	tns := analyzeTNS(encoded, offsets, 4, false)
	if got := len(tns.windows); got != 1 {
		t.Fatalf("windows = %d, want 1", got)
	}
	if got := len(tns.windows[0].filters); got != 1 {
		t.Fatalf("filters = %d, want 1", got)
	}
	filter := tns.windows[0].filters[0]
	if got := len(filter.coefficients); got == 0 || got > 12 {
		t.Fatalf("order = %d, want 1..12", got)
	}
	if filter.length != len(offsets)-1 {
		t.Fatalf("length = %d, want %d", filter.length, len(offsets)-1)
	}
	for i, code := range filter.coefficientCodes {
		if got, want := filter.coefficients[i], tables.TNSCoefficients[1][code]; got != want {
			t.Fatalf("coefficient[%d] = %g, table[%d] = %g", i, got, code, want)
		}
	}
	if slicesEqual(encoded, original) {
		t.Fatal("analysis filter left predictable spectrum unchanged")
	}

	applyTNS(encoded, tns, 0, offsets, 4)
	checkFloatSlice(t, encoded, original, 1e-9)
}

func TestAnalyzeTNSShortWindows(t *testing.T) {
	offsets := []uint16{0, 16, 32, 64, 96, 128}
	original := make([]float64, 8*128)
	for w := range 8 {
		copy(original[w*128:], predictiveSpectrum(128, 0.88, uint32(31+w)))
	}
	encoded := append([]float64(nil), original...)
	tns := analyzeTNS(encoded, offsets, 5, true)

	if got := len(tns.windows); got != 8 {
		t.Fatalf("windows = %d, want 8", got)
	}
	for w, window := range tns.windows {
		if len(window.filters) != 1 {
			t.Fatalf("window %d filters = %d, want 1", w, len(window.filters))
		}
		if order := len(window.filters[0].coefficients); order == 0 || order > 7 {
			t.Fatalf("window %d order = %d, want 1..7", w, order)
		}
		applyTNS(encoded, tns, w, offsets, 5)
	}
	checkFloatSlice(t, encoded, original, 1e-9)
}

func TestAnalyzeTNSOmitsUnpredictiveSpectrum(t *testing.T) {
	spec := make([]float64, 1024)
	spec[512] = 1
	tns := analyzeTNS(spec, []uint16{0, 256, 512, 768, 1024}, 4, false)
	if len(tns.windows[0].filters) != 0 {
		t.Fatalf("filters = %+v, want none at prediction gain 1", tns.windows[0].filters)
	}
}

func TestWriteTNSDataRoundTrip(t *testing.T) {
	table := tables.TNSCoefficients[1]
	tns := &tnsData{windows: []tnsWindow{{filters: []tnsFilter{{
		length:           7,
		direction:        true,
		coefficients:     []float64{table[2], table[9], table[15]},
		coefficientCodes: []uint8{2, 9, 15},
	}}}}}
	w := bits.NewWriter()
	if err := writeTNSData(w, tns, false); err != nil {
		t.Fatalf("writeTNSData: %v", err)
	}
	got, err := parseTNS(bits.NewReader(w.Bytes()), onlyLongSequence)
	if err != nil {
		t.Fatalf("parseTNS: %v", err)
	}
	filter := got.windows[0].filters[0]
	if filter.length != 7 || !filter.direction {
		t.Fatalf("filter = %+v, want length 7 and reverse direction", filter)
	}
	checkFloatSlice(t, filter.coefficients, []float64{table[2], table[9], table[15]}, 0)
}

func TestEncoderTNSOptInRoundTrip(t *testing.T) {
	const sampleRate = 44100
	pcm := make([]int16, sampleRate/4)
	for i := range pcm {
		pcm[i] = int16(18000 * math.Sin(2*math.Pi*440*float64(i)/sampleRate))
	}
	encode := func(enableTNS bool) []byte {
		encoder, err := NewEncoder(Config{
			SampleRate: sampleRate,
			Channels:   1,
			Quality:    0.5,
			EnableTNS:  enableTNS,
		})
		if err != nil {
			t.Fatal(err)
		}
		data, err := encoder.Encode(pcm)
		if err != nil {
			t.Fatal(err)
		}
		tail, err := encoder.Flush()
		if err != nil {
			t.Fatal(err)
		}
		return append(data, tail...)
	}

	withoutTNS := encode(false)
	withTNS := encode(true)
	if bytes.Equal(withTNS, withoutTNS) {
		t.Fatal("EnableTNS did not change the encoded stream")
	}
	decoder, err := NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decoder.Decode(withTNS)
	if err != nil {
		t.Fatalf("decode TNS stream: %v", err)
	}
	if len(decoded) < len(pcm) {
		t.Fatalf("decoded %d samples, want at least %d", len(decoded), len(pcm))
	}
}

func predictiveSpectrum(n int, pole float64, seed uint32) []float64 {
	values := make([]float64, n)
	for i := range values {
		seed = seed*1664525 + 1013904223
		innovation := float64(int32(seed>>16)-32768) / 32768
		if i > 0 {
			values[i] = pole*values[i-1] + innovation
		} else {
			values[i] = innovation
		}
	}
	return values
}

func slicesEqual(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestApplyMidSide(t *testing.T) {
	left := []float64{1, -2, 3}
	right := []float64{0.5, 4, -1}
	applyMidSide(left, right)
	checkFloatSlice(t, left, []float64{1.5, 2, 2}, 0)
	checkFloatSlice(t, right, []float64{0.5, -6, 4}, 0)
}

func TestApplyIntensity(t *testing.T) {
	left := []float64{2, -4, 8}
	right := make([]float64, len(left))
	applyIntensity(left, right, 4, false)
	checkFloatSlice(t, right, []float64{1, -2, 4}, 1e-15)
	applyIntensity(left, right, 0, true)
	checkFloatSlice(t, right, []float64{-2, 4, -8}, 1e-15)
}

func TestFillPNS(t *testing.T) {
	for _, tc := range []struct {
		noiseEnergy int
		wantNorm    float64
	}{
		{noiseEnergy: 0, wantNorm: 1},
		{noiseEnergy: 4, wantNorm: 2},
		{noiseEnergy: -4, wantNorm: 0.5},
	} {
		band := make([]float64, 257)
		fillPNS(band, tc.noiseEnergy, rand.New(rand.NewSource(7)))
		var energy float64
		for _, value := range band {
			energy += value * value
		}
		if got := math.Sqrt(energy); math.Abs(got-tc.wantNorm) > 1e-12 {
			t.Fatalf("noiseEnergy %d: norm = %.15g, want %.15g", tc.noiseEnergy, got, tc.wantNorm)
		}
	}

	one := make([]float64, 16)
	two := make([]float64, 16)
	fillPNS(one, 0, rand.New(rand.NewSource(99)))
	fillPNS(two, 0, rand.New(rand.NewSource(99)))
	checkFloatSlice(t, one, two, 0)

	fillPNS(nil, 0, rand.New(rand.NewSource(1)))
}

func writeTestBits(t *testing.T, w *bits.BitWriter, value uint64, width int) {
	t.Helper()
	if err := w.WriteBits(value, width); err != nil {
		t.Fatalf("WriteBits(%d, %d): %v", value, width, err)
	}
}

func checkFloatSlice(t *testing.T, got, want []float64, tolerance float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("length = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if math.Abs(got[i]-want[i]) > tolerance {
			t.Fatalf("value %d = %.15g, want %.15g", i, got[i], want[i])
		}
	}
}

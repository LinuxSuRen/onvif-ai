package aac_test

import (
	"math"
	"testing"

	aac "github.com/arabian9ts/aac-go"
)

// TestQualityKnobMonotonic pins the Quality contract:
// across the knob's range, own-decoded SNR must strictly increase and the
// encoded size must not shrink. The original knob bug — the DPCM clamp
// binding before the quality parameter, making q=0.0 and q=0.5 byte-
// identical on tonal input — was exactly the absence of this test.
func TestQualityKnobMonotonic(t *testing.T) {
	const (
		sampleRate = 44100
		samples    = sampleRate / 2
	)
	pcm := make([]int16, samples)
	for i := range pcm {
		ts := float64(i) / sampleRate
		v := 0.4*math.Sin(2*math.Pi*440*ts) + 0.3*math.Sin(2*math.Pi*1320*ts) + 0.15*math.Sin(2*math.Pi*5200*ts)
		pcm[i] = int16(v * 30000)
	}

	prevSNR := math.Inf(-1)
	prevSize := 0
	for _, quality := range []float64{0, 0.25, 0.5, 0.75, 1.0} {
		enc, err := aac.NewEncoder(aac.Config{SampleRate: sampleRate, Channels: 1, Quality: quality})
		if err != nil {
			t.Fatalf("NewEncoder(q=%.2f): %v", quality, err)
		}
		data, err := enc.Encode(pcm)
		if err != nil {
			t.Fatalf("Encode(q=%.2f): %v", quality, err)
		}
		tail, err := enc.Flush()
		if err != nil {
			t.Fatalf("Flush(q=%.2f): %v", quality, err)
		}
		data = append(data, tail...)

		dec, err := aac.NewDecoder()
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := dec.Decode(data)
		if err != nil {
			t.Fatalf("Decode(q=%.2f): %v", quality, err)
		}

		snr := delaySNR(pcm, decoded, aac.SamplesPerFrame)
		t.Logf("quality %.2f: %d bytes, %.1f dB", quality, len(data), snr)
		if snr <= prevSNR+0.5 {
			t.Fatalf("quality %.2f: SNR %.1f dB does not improve on previous %.1f dB", quality, snr, prevSNR)
		}
		if len(data) < prevSize {
			t.Fatalf("quality %.2f: size %d shrank below previous %d", quality, len(data), prevSize)
		}
		prevSNR, prevSize = snr, len(data)
	}
}

// delaySNR compares source and decode aligned by the fixed encoder delay.
func delaySNR(want, got []int16, delay int) float64 {
	var sig, noise float64
	for i := range want {
		if i+delay >= len(got) {
			break
		}
		s := float64(want[i])
		d := s - float64(got[i+delay])
		sig += s * s
		noise += d * d
	}
	if noise == 0 {
		return math.Inf(1)
	}
	return 10 * math.Log10(sig/noise)
}

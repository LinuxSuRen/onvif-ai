package wav

import (
	"bytes"
	"math"
	"testing"
)

func TestRoundtrip(t *testing.T) {
	cases := []struct {
		name       string
		sampleRate int
		channels   int
		samples    int
	}{
		{"mono empty", 44100, 1, 0},
		{"mono short", 44100, 1, 3},
		{"stereo", 48000, 2, 2048},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pcm := make([]int16, tc.samples*tc.channels)
			for i := range pcm {
				pcm[i] = int16(math.MaxInt16 * math.Sin(float64(i)/17))
			}
			var buf bytes.Buffer
			if err := Write(&buf, tc.sampleRate, tc.channels, pcm); err != nil {
				t.Fatalf("Write: %v", err)
			}
			sr, ch, got, err := Read(&buf)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if sr != tc.sampleRate || ch != tc.channels {
				t.Fatalf("format mismatch: got %d Hz %d ch, want %d Hz %d ch", sr, ch, tc.sampleRate, tc.channels)
			}
			if !bytes.Equal(int16ToBytes(got), int16ToBytes(pcm)) {
				t.Fatal("PCM samples do not roundtrip")
			}
		})
	}
}

func TestReadRejectsNonWave(t *testing.T) {
	if _, _, _, err := Read(bytes.NewReader([]byte("not a wave file at all"))); err == nil {
		t.Fatal("expected error for non-WAVE input")
	}
}

func int16ToBytes(pcm []int16) []byte {
	out := make([]byte, len(pcm)*2)
	for i, s := range pcm {
		out[i*2] = byte(uint16(s))
		out[i*2+1] = byte(uint16(s) >> 8)
	}
	return out
}

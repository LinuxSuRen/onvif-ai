package aac

import (
	"os"
	"testing"
)

// FuzzDecode asserts the decoder never panics on arbitrary input — errors
// are the only acceptable failure mode for malformed streams. Run with
// `go test -fuzz=FuzzDecode` for continuous exploration; the seed corpus
// (a valid silent stream and mutations of it) runs in normal `go test`.
func FuzzDecode(f *testing.F) {
	enc, err := NewEncoder(Config{SampleRate: 44100, Channels: 1, Quality: 0.5})
	if err != nil {
		f.Fatalf("NewEncoder: %v", err)
	}
	valid, err := enc.Encode(make([]int16, 3*SamplesPerFrame))
	if err != nil {
		f.Fatalf("Encode: %v", err)
	}
	f.Add(valid)
	if len(valid) > 9 {
		truncated := valid[:len(valid)-5]
		f.Add(truncated)
		corrupt := append([]byte(nil), valid...)
		corrupt[9] ^= 0xff
		f.Add(corrupt)
	}
	f.Add([]byte{0xff, 0xf1})
	f.Add([]byte{})
	// Real-world seed exercising short windows, grouping, and TNS parsing.
	if rw, err := os.ReadFile("testdata/realworld/off_mono.aac"); err == nil {
		f.Add(rw)
	}
	if rw, err := os.ReadFile("testdata/realworld/default_stereo.aac"); err == nil {
		f.Add(rw)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		dec, err := NewDecoder()
		if err != nil {
			t.Fatalf("NewDecoder: %v", err)
		}
		_, _ = dec.Decode(data) // must not panic
	})
}

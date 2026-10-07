package aac

import (
	"testing"
)

// TestDecodeSilenceRoundtrip decodes the M1 silent encoder's output: every
// sample must be zero and the format must be detected from the ADTS header.
func TestDecodeSilenceRoundtrip(t *testing.T) {
	enc, err := NewEncoder(Config{SampleRate: 44100, Channels: 1, Quality: 0.5})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	data, err := enc.Encode(make([]int16, 4*SamplesPerFrame))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	tail, err := enc.Flush()
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	data = append(data, tail...)

	dec, err := NewDecoder()
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	pcm, err := dec.Decode(data)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if dec.SampleRate() != 44100 || dec.Channels() != 1 {
		t.Fatalf("format = %d Hz %d ch, want 44100 Hz 1 ch", dec.SampleRate(), dec.Channels())
	}
	if len(pcm) < 4*SamplesPerFrame {
		t.Fatalf("decoded %d samples, want at least %d", len(pcm), 4*SamplesPerFrame)
	}
	for i, s := range pcm {
		if s != 0 {
			t.Fatalf("sample %d = %d, want 0", i, s)
		}
	}
}

// TestDecodeByteAtATime feeds the stream one byte per call to exercise
// partial-frame buffering.
func TestDecodeByteAtATime(t *testing.T) {
	enc, err := NewEncoder(Config{SampleRate: 48000, Channels: 1, Quality: 0.5})
	if err != nil {
		t.Fatalf("NewEncoder: %v", err)
	}
	data, err := enc.Encode(make([]int16, 2*SamplesPerFrame))
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	tail, err := enc.Flush()
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	data = append(data, tail...)

	dec, err := NewDecoder()
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	var total int
	for i := range data {
		pcm, err := dec.Decode(data[i : i+1])
		if err != nil {
			t.Fatalf("Decode at byte %d: %v", i, err)
		}
		total += len(pcm)
	}
	if total < 2*SamplesPerFrame {
		t.Fatalf("decoded %d samples, want at least %d", total, 2*SamplesPerFrame)
	}
}

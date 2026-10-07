package aac_test

import (
	"math"
	"strconv"
	"testing"

	aac "github.com/arabian9ts/aac-go"
	"github.com/arabian9ts/aac-go/adts"
)

// TestEncodeLengthIndependence exercises the streaming contract: arbitrary
// input lengths — including empty, sub-frame, and off-by-one around the 1024-sample frame size — must yield a structurally
// valid ADTS stream covering at least every input sample.
func TestEncodeLengthIndependence(t *testing.T) {
	lengths := []int{0, 1, 1023, 1024, 1025, 2048, 4800, 44100}
	for _, n := range lengths {
		t.Run("len_"+strconv.Itoa(n), func(t *testing.T) {
			enc, err := aac.NewEncoder(aac.Config{SampleRate: 44100, Channels: 1, Quality: 0.5})
			if err != nil {
				t.Fatalf("NewEncoder: %v", err)
			}
			pcm := make([]int16, n)
			for i := range pcm {
				pcm[i] = int16(20000 * math.Sin(float64(i)/23))
			}
			data, err := enc.Encode(pcm)
			if err != nil {
				t.Fatalf("Encode: %v", err)
			}
			tail, err := enc.Flush()
			if err != nil {
				t.Fatalf("Flush: %v", err)
			}
			data = append(data, tail...)

			frames := countADTSFrames(t, data)
			if minFrames := (n + 1023) / 1024; frames < minFrames {
				t.Fatalf("%d samples produced %d frames, need at least %d", n, frames, minFrames)
			}
		})
	}
}

// TestEncodeChunkingInvariance feeds the same signal in different chunk
// sizes; the concatenated ADTS output must be identical regardless of how
// the caller slices the input.
func TestEncodeChunkingInvariance(t *testing.T) {
	const total = 5000
	pcm := make([]int16, total)
	for i := range pcm {
		pcm[i] = int16(15000 * math.Sin(float64(i)/11))
	}

	encode := func(chunk int) []byte {
		enc, err := aac.NewEncoder(aac.Config{SampleRate: 48000, Channels: 1, Quality: 0.5})
		if err != nil {
			t.Fatalf("NewEncoder: %v", err)
		}
		var out []byte
		for off := 0; off < len(pcm); off += chunk {
			end := min(off+chunk, len(pcm))
			part, err := enc.Encode(pcm[off:end])
			if err != nil {
				t.Fatalf("Encode(chunk %d): %v", chunk, err)
			}
			out = append(out, part...)
		}
		tail, err := enc.Flush()
		if err != nil {
			t.Fatalf("Flush(chunk %d): %v", chunk, err)
		}
		return append(out, tail...)
	}

	want := encode(total)
	for _, chunk := range []int{1, 7, 1024, 1500} {
		if got := encode(chunk); string(got) != string(want) {
			t.Fatalf("chunk size %d produced different output (%d vs %d bytes)", chunk, len(got), len(want))
		}
	}
}

// TestFrameSizeLimit encodes worst-case signals (dense noise, maximum
// quality) and asserts every ADTS frame respects the AAC-LC decoder input
// buffer limit of 6144 bits per channel (ISO/IEC 14496-3 §4.5.3). Streams
// exceeding it are rejected by conformant decoders even though they parse.
func TestFrameSizeLimit(t *testing.T) {
	for _, channels := range []int{1, 2} {
		enc, err := aac.NewEncoder(aac.Config{SampleRate: 44100, Channels: channels, Quality: 1.0})
		if err != nil {
			t.Fatalf("NewEncoder: %v", err)
		}
		pcm := make([]int16, 8*1024*channels)
		x := uint32(1)
		for i := range pcm {
			x ^= x << 13
			x ^= x >> 17
			x ^= x << 5
			pcm[i] = int16(uint16(x)) //nolint:gosec
		}
		data, err := enc.Encode(pcm)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		tail, err := enc.Flush()
		if err != nil {
			t.Fatalf("Flush: %v", err)
		}
		data = append(data, tail...)

		maxFrameBytes := 6144/8*channels + adts.HeaderSize
		frameIndex := 0
		for off := 0; off < len(data); frameIndex++ {
			h, err := adts.Parse(data[off:])
			if err != nil {
				t.Fatalf("channels=%d frame %d: %v", channels, frameIndex, err)
			}
			if h.FrameLength > maxFrameBytes {
				t.Fatalf("channels=%d frame %d is %d bytes, exceeds AAC-LC limit %d",
					channels, frameIndex, h.FrameLength, maxFrameBytes)
			}
			off += h.FrameLength
		}
	}
}

// countADTSFrames walks data as consecutive ADTS frames and fails the test
// on any structural inconsistency or trailing garbage.
func countADTSFrames(t *testing.T, data []byte) int {
	t.Helper()
	frames := 0
	for off := 0; off < len(data); {
		h, err := adts.Parse(data[off:])
		if err != nil {
			t.Fatalf("frame %d at byte %d: %v", frames, off, err)
		}
		if off+h.FrameLength > len(data) {
			t.Fatalf("frame %d at byte %d: length %d overruns stream", frames, off, h.FrameLength)
		}
		off += h.FrameLength
		frames++
	}
	return frames
}

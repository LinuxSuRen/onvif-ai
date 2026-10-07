package aac

import (
	"math"
	"testing"

	"github.com/arabian9ts/aac-go/adts"
	"github.com/arabian9ts/aac-go/internal/bits"
)

// castanetPCM builds an impulse-train test signal: sharp wideband attacks
// every 250 ms over a quiet tonal bed — the classic pre-echo material that
// block switching exists for.
func castanetPCM(sampleRate, channels, samples int) []int16 {
	pcm := make([]int16, samples*channels)
	for i := range samples {
		ts := float64(i) / float64(sampleRate)
		v := 0.05 * math.Sin(2*math.Pi*220*ts)
		if phase := i % (sampleRate / 4); phase < 256 {
			decay := math.Exp(-float64(phase) / 40)
			v += 0.85 * decay * math.Sin(2*math.Pi*3100*float64(phase)/float64(sampleRate))
		}
		for ch := range channels {
			pcm[i*channels+ch] = int16(v * 32000)
		}
	}
	return pcm
}

// frameSequences parses the window_sequence of every frame's first element
// (SCE or CPE with common window: the bit layout up to window_sequence is
// identical — id(3) tag(4) [common_window(1) for CPE] ... but global_gain
// precedes ics_info only in the SCE/ICS path, so parse per element type).
func frameSequences(t *testing.T, data []byte) []int {
	t.Helper()
	var sequences []int
	for off := 0; off < len(data); {
		h, err := adts.Parse(data[off:])
		if err != nil {
			t.Fatalf("frame at %d: %v", off, err)
		}
		r := bits.NewReader(data[off+h.PayloadOffset() : off+h.FrameLength])
		id, _ := r.ReadBits(3)
		if _, err := r.ReadBits(4); err != nil { // element_instance_tag
			t.Fatal(err)
		}
		switch id {
		case idSCE:
			if _, err := r.ReadBits(8); err != nil { // global_gain
				t.Fatal(err)
			}
		case idCPE:
			if _, err := r.ReadBits(1); err != nil { // common_window
				t.Fatal(err)
			}
		default:
			t.Fatalf("unexpected element id %d", id)
		}
		if _, err := r.ReadBits(1); err != nil { // ics_reserved_bit
			t.Fatal(err)
		}
		seq, err := r.ReadBits(2)
		if err != nil {
			t.Fatal(err)
		}
		sequences = append(sequences, int(seq))
		off += h.FrameLength
	}
	return sequences
}

// TestBlockSwitchingChain encodes transient material and asserts that the
// encoder both uses EIGHT_SHORT windows and emits a well-formed sequence
// chain: EIGHT_SHORT is entered via LONG_START and left via LONG_STOP.
func TestBlockSwitchingChain(t *testing.T) {
	for _, channels := range []int{1, 2} {
		enc, err := NewEncoder(Config{SampleRate: 44100, Channels: channels, Quality: 0.5})
		if err != nil {
			t.Fatal(err)
		}
		pcm := castanetPCM(44100, channels, 44100)
		data, err := enc.Encode(pcm)
		if err != nil {
			t.Fatal(err)
		}
		tail, err := enc.Flush()
		if err != nil {
			t.Fatal(err)
		}
		data = append(data, tail...)

		sequences := frameSequences(t, data)
		shorts := 0
		prev := onlyLongSequence
		for i, seq := range sequences {
			switch seq {
			case eightShortSequence:
				shorts++
				if prev != longStartSequence && prev != eightShortSequence {
					t.Fatalf("channels=%d frame %d: EIGHT_SHORT after sequence %d", channels, i, prev)
				}
			case longStopSequence:
				if prev != eightShortSequence && prev != longStartSequence {
					t.Fatalf("channels=%d frame %d: LONG_STOP after sequence %d", channels, i, prev)
				}
			case onlyLongSequence, longStartSequence:
				if prev == eightShortSequence || prev == longStartSequence {
					t.Fatalf("channels=%d frame %d: sequence %d directly after %d", channels, i, seq, prev)
				}
			}
			prev = seq
		}
		if prev == eightShortSequence || prev == longStartSequence {
			t.Fatalf("channels=%d: stream ends inside a short run (sequence %d)", channels, prev)
		}
		if shorts == 0 {
			t.Fatalf("channels=%d: transient material produced no EIGHT_SHORT frames", channels)
		}

		// The full roundtrip must still decode cleanly.
		dec, err := NewDecoder()
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := dec.Decode(data)
		if err != nil {
			t.Fatalf("channels=%d: decode: %v", channels, err)
		}
		if len(decoded) < len(pcm) {
			t.Fatalf("channels=%d: decoded %d samples, want at least %d", channels, len(decoded), len(pcm))
		}
	}
}

// TestSteadySignalsStayLong pins the detector's other side: tonal and
// noise-like steady signals must not trigger short windows.
func TestSteadySignalsStayLong(t *testing.T) {
	enc, err := NewEncoder(Config{SampleRate: 44100, Channels: 1, Quality: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	pcm := make([]int16, 44100)
	x := uint32(9)
	for i := range pcm {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		noise := float64(int32(x))/float64(1<<31)*0.2 + 0.6*math.Sin(2*math.Pi*440*float64(i)/44100)
		pcm[i] = int16(noise * 30000)
	}
	data, err := enc.Encode(pcm)
	if err != nil {
		t.Fatal(err)
	}
	tail, err := enc.Flush()
	if err != nil {
		t.Fatal(err)
	}
	for i, seq := range frameSequences(t, append(data, tail...)) {
		if seq != onlyLongSequence {
			t.Fatalf("frame %d: steady signal produced sequence %d", i, seq)
		}
	}
}

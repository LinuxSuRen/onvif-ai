package aac

import (
	"testing"

	"github.com/arabian9ts/aac-go/adts"
	"github.com/arabian9ts/aac-go/internal/bits"
	"github.com/arabian9ts/aac-go/internal/tables"
)

// TestDecodeSilentCPE decodes a hand-written silent stereo frame: one CPE
// with common_window=1, ms_mask_present=0, and all bands ZERO_HCB in both
// channels. This pins the CPE syntax path ahead of the M4 stereo encoder.
func TestDecodeSilentCPE(t *testing.T) {
	const samplingFrequencyIndex = 4 // 44.1 kHz
	offsets, ok := tables.LongWindowSFBOffsets(samplingFrequencyIndex)
	if !ok {
		t.Fatal("no SFB table")
	}
	maxSFB := uint64(len(offsets) - 1)

	w := bits.NewWriter()
	write := func(v uint64, n int) {
		t.Helper()
		if err := w.WriteBits(v, n); err != nil {
			t.Fatalf("WriteBits(%d, %d): %v", v, n, err)
		}
	}

	write(idCPE, 3)
	write(0, 4) // element_instance_tag
	write(1, 1) // common_window
	// Shared ics_info() — ISO/IEC 14496-3 Table 4.6.
	write(0, 1)      // ics_reserved_bit
	write(0, 2)      // window_sequence: ONLY_LONG
	write(0, 1)      // window_shape: sine
	write(maxSFB, 6) // max_sfb
	write(0, 1)      // predictor_data_present
	write(0, 2)      // ms_mask_present: none
	for ch := 0; ch < 2; ch++ {
		write(0, 8)         // global_gain
		write(zeroHCB, 4)   // section_data: one ZERO_HCB run
		write(31, 5)        // sect_len_incr escape
		write(maxSFB-31, 5) // remainder
		write(0, 1)         // pulse_data_present
		write(0, 1)         // tns_data_present
		write(0, 1)         // gain_control_data_present
	}
	write(idEND, 3)
	w.Align()
	payload := w.Bytes()

	header, err := adts.Build(adts.Header{
		Profile:                1,
		SamplingFrequencyIndex: samplingFrequencyIndex,
		ChannelConfiguration:   2,
		FrameLength:            adts.HeaderSize + len(payload),
		BufferFullness:         adts.VariableBitRateBufferFullness,
	})
	if err != nil {
		t.Fatalf("adts.Build: %v", err)
	}

	dec, err := NewDecoder()
	if err != nil {
		t.Fatalf("NewDecoder: %v", err)
	}
	pcm, err := dec.Decode(append(header, payload...))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if dec.Channels() != 2 || dec.SampleRate() != 44100 {
		t.Fatalf("format = %d Hz %d ch, want 44100 Hz 2 ch", dec.SampleRate(), dec.Channels())
	}
	if len(pcm) != 2*SamplesPerFrame {
		t.Fatalf("decoded %d samples, want %d", len(pcm), 2*SamplesPerFrame)
	}
	for i, s := range pcm {
		if s != 0 {
			t.Fatalf("sample %d = %d, want 0", i, s)
		}
	}
}

package aac

import (
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/arabian9ts/aac-go/adts"
)

func TestSilentEncoderStreaming(t *testing.T) {
	encoder, err := NewEncoder(Config{SampleRate: 44100, Channels: 1})
	if err != nil {
		t.Fatal(err)
	}
	if data, err := encoder.Encode(make([]int16, 1023)); err != nil || len(data) != 0 {
		t.Fatalf("first Encode = (%d bytes, %v), want (0, nil)", len(data), err)
	}
	data, err := encoder.Encode(make([]int16, 1026))
	if err != nil {
		t.Fatal(err)
	}
	frames := splitFrames(t, data)
	// Two blocks completed, but the block-switching lookahead holds the
	// latest one back.
	if got, want := len(frames), 1; got != want {
		t.Fatalf("second Encode returned %d frames, want %d", got, want)
	}
	for i, frame := range frames {
		assertSilentFrame(t, i, frame, 4)
	}
	data, err = encoder.Flush()
	if err != nil {
		t.Fatal(err)
	}
	frames = splitFrames(t, data)
	// The pipelined frame, the zero-padded partial block, and the MDCT
	// tail frame.
	if got, want := len(frames), 3; got != want {
		t.Fatalf("Flush returned %d frames, want %d", got, want)
	}
	assertSilentFrame(t, 2, frames[0], 4)
	if _, err := encoder.Flush(); !errors.Is(err, ErrEncoderFlushed) {
		t.Fatalf("second Flush error = %v, want ErrEncoderFlushed", err)
	}
	if _, err := encoder.Encode(nil); !errors.Is(err, ErrEncoderFlushed) {
		t.Fatalf("Encode after Flush error = %v, want ErrEncoderFlushed", err)
	}
}

func TestSilentEncoderOneSecond(t *testing.T) {
	encoder, err := NewEncoder(Config{SampleRate: 44100, Channels: 1})
	if err != nil {
		t.Fatal(err)
	}
	data, err := encoder.Encode(make([]int16, 44100))
	if err != nil {
		t.Fatal(err)
	}
	final, err := encoder.Flush()
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, final...)
	frames := splitFrames(t, data)
	// ceil(44100/1024) = 44 blocks plus the MDCT tail frame from Flush.
	if got, want := len(frames), 45; got != want {
		t.Fatalf("got %d frames, want %d", got, want)
	}
}

func TestEncoderKeepsOnlyPartialFrame(t *testing.T) {
	encoder, err := NewEncoder(Config{SampleRate: 48000, Channels: 1})
	if err != nil {
		t.Fatal(err)
	}
	data, err := encoder.Encode(make([]int16, SamplesPerFrame*100+7))
	if err != nil {
		t.Fatal(err)
	}
	// One frame stays in the lookahead pipeline until Flush.
	if got, want := len(splitFrames(t, data)), 99; got != want {
		t.Fatalf("got %d frames, want %d", got, want)
	}
	if got, want := len(encoder.pending), 7; got != want {
		t.Fatalf("pending length = %d, want %d", got, want)
	}
	if got, want := cap(encoder.pending), SamplesPerFrame; got != want {
		t.Fatalf("pending capacity = %d, want %d", got, want)
	}
}

func TestStereoEncoderRoundTrip(t *testing.T) {
	const (
		sampleRate = 48000
		pcmFrames  = 4 * SamplesPerFrame
	)
	pcm := make([]int16, pcmFrames*2)
	for i := range pcmFrames {
		seconds := float64(i) / sampleRate
		pcm[2*i] = int16(14000 * math.Sin(2*math.Pi*440*seconds))
		pcm[2*i+1] = int16(11000 * math.Sin(2*math.Pi*997*seconds+math.Pi/3))
	}

	encoder, err := NewEncoder(Config{SampleRate: sampleRate, Channels: 2})
	if err != nil {
		t.Fatal(err)
	}
	data, err := encoder.Encode(pcm[:17]) // split within an interleaved frame
	if err != nil {
		t.Fatal(err)
	}
	rest, err := encoder.Encode(pcm[17:])
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, rest...)
	tail, err := encoder.Flush()
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, tail...)

	frames := splitFrames(t, data)
	if got, want := len(frames), pcmFrames/SamplesPerFrame+1; got != want {
		t.Fatalf("got %d ADTS frames, want %d", got, want)
	}
	for i, frame := range frames {
		header, err := adts.Parse(frame)
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if header.ChannelConfiguration != 2 {
			t.Fatalf("frame %d channel_configuration = %d, want 2", i, header.ChannelConfiguration)
		}
	}

	decoder, err := NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decoder.Decode(data)
	if err != nil {
		t.Fatal(err)
	}
	if decoder.SampleRate() != sampleRate || decoder.Channels() != 2 {
		t.Fatalf("decoded format = %d Hz/%d ch, want %d Hz/2 ch", decoder.SampleRate(), decoder.Channels(), sampleRate)
	}
	for ch := range 2 {
		if got := stereoChannelSNR(pcm, decoded, ch, SamplesPerFrame); got < 25 {
			t.Fatalf("channel %d SNR = %.1f dB, want at least 25 dB", ch, got)
		}
	}
}

func TestEncoderEnforcesAACLCFrameSizeLimit(t *testing.T) {
	for _, channels := range []int{1, 2} {
		for _, quality := range []float64{0.5, 1} {
			t.Run(fmt.Sprintf("%dch/quality%.1f", channels, quality), func(t *testing.T) {
				const pcmFrames = 4 * SamplesPerFrame
				pcm := make([]int16, pcmFrames*channels)
				state := uint32(1)
				for i := range pcm {
					// Deterministic high-entropy input exercises the cap-reduction path.
					state = state*1664525 + 1013904223
					pcm[i] = int16(state >> 16)
				}

				encoder, err := NewEncoder(Config{SampleRate: 44100, Channels: channels, Quality: quality})
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
				data = append(data, tail...)
				for frameNumber, frame := range splitFrames(t, data) {
					if got, limit := len(frame)*8, maxFrameBitsPerChannel*channels; got > limit {
						t.Fatalf("frame %d uses %d bits, limit is %d", frameNumber, got, limit)
					}
				}
			})
		}
	}
}

func TestNewEncoderValidation(t *testing.T) {
	for _, config := range []Config{
		{SampleRate: 32000, Channels: 1},
		{SampleRate: 44100, Channels: 0},
		{SampleRate: 44100, Channels: 3},
		{SampleRate: 44100, Channels: 1, Quality: -0.1},
		{SampleRate: 44100, Channels: 1, Quality: 1.1},
		{SampleRate: 44100, Channels: 1, Quality: math.NaN()},
	} {
		if _, err := NewEncoder(config); err == nil {
			t.Fatalf("NewEncoder(%+v) succeeded", config)
		}
	}
}

func assertSilentFrame(t *testing.T, number int, frame []byte, frequencyIndex uint8) {
	t.Helper()
	header, err := adts.Parse(frame)
	if err != nil {
		t.Fatalf("frame %d: Parse: %v", number, err)
	}
	if header.Profile != 1 || header.SamplingFrequencyIndex != frequencyIndex || header.ChannelConfiguration != 1 {
		t.Fatalf("frame %d: unexpected header %+v", number, header)
	}
	if header.FrameLength != len(frame) {
		t.Fatalf("frame %d: header length %d, actual %d", number, header.FrameLength, len(frame))
	}
}

func splitFrames(t *testing.T, data []byte) [][]byte {
	t.Helper()
	var frames [][]byte
	for len(data) != 0 {
		header, err := adts.Parse(data)
		if err != nil {
			t.Fatalf("parse concatenated ADTS frame %d: %v", len(frames), err)
		}
		if header.FrameLength > len(data) {
			t.Fatalf("frame %d length = %d, only %d bytes remain", len(frames), header.FrameLength, len(data))
		}
		frames = append(frames, data[:header.FrameLength])
		data = data[header.FrameLength:]
	}
	return frames
}

func stereoChannelSNR(want, got []int16, channel, delayFrames int) float64 {
	var signal, noise float64
	for frame := 0; frame < len(want)/2; frame++ {
		gotIndex := (frame+delayFrames)*2 + channel
		if gotIndex >= len(got) {
			break
		}
		wantSample := float64(want[frame*2+channel])
		delta := wantSample - float64(got[gotIndex])
		signal += wantSample * wantSample
		noise += delta * delta
	}
	if noise == 0 {
		return math.Inf(1)
	}
	return 10 * math.Log10(signal/noise)
}

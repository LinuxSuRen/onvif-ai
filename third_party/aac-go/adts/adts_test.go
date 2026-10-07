package adts

import (
	"errors"
	"reflect"
	"testing"
)

func TestHeaderRoundTrip(t *testing.T) {
	for _, want := range []Header{
		{Profile: 1, SamplingFrequencyIndex: 4, ChannelConfiguration: 1, FrameLength: 13, BufferFullness: VariableBitRateBufferFullness},
		{MPEGID: 1, Profile: 2, SamplingFrequencyIndex: 3, ChannelConfiguration: 2, FrameLength: MaxFrameLength, BufferFullness: 123, NumRawDataBlocks: 3},
		{Profile: 1, SamplingFrequencyIndex: 4, ChannelConfiguration: 0, FrameLength: 32, BufferFullness: VariableBitRateBufferFullness},
	} {
		encoded, err := Build(want)
		if err != nil {
			t.Fatalf("Build(%+v): %v", want, err)
		}
		if len(encoded) != HeaderSize {
			t.Fatalf("header size = %d, want %d", len(encoded), HeaderSize)
		}
		got, err := Parse(encoded)
		if err != nil {
			t.Fatalf("Parse(% x): %v", encoded, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("roundtrip got %+v, want %+v", got, want)
		}
	}
}

func TestParseKnownAACLCHeader(t *testing.T) {
	got, err := Parse([]byte{0xff, 0xf1, 0x50, 0x40, 0x01, 0xbf, 0xfc})
	if err != nil {
		t.Fatal(err)
	}
	want := Header{Profile: 1, SamplingFrequencyIndex: 4, ChannelConfiguration: 1, FrameLength: 13, BufferFullness: VariableBitRateBufferFullness}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestParseRejectsInvalidHeaders(t *testing.T) {
	if _, err := Parse(make([]byte, 6)); !errors.Is(err, ErrShortHeader) {
		t.Fatalf("short header error = %v", err)
	}
	if _, err := Parse([]byte{0, 0, 0, 0, 0, 0, 0}); !errors.Is(err, ErrSyncWord) {
		t.Fatalf("sync error = %v", err)
	}
	if h, err := Parse([]byte{0xff, 0xf0, 0x50, 0x40, 0x01, 0xbf, 0xfc}); err != nil || !h.CRCPresent || h.PayloadOffset() != HeaderSize+CRCSize {
		t.Fatalf("CRC error = %v", err)
	}
}

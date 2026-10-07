// Package adts builds and parses Audio Data Transport Stream headers.
package adts

import (
	"errors"
	"fmt"
)

const (
	// HeaderSize is the size of an ADTS header without a CRC.
	HeaderSize = 7
	// CRCSize is the size of the optional CRC that follows the header when
	// protection_absent is zero (one raw data block per frame).
	CRCSize = 2
	// MaxFrameLength is the largest value representable by adts_frame_length.
	MaxFrameLength = 1<<13 - 1
	// VariableBitRateBufferFullness marks a variable-rate stream.
	VariableBitRateBufferFullness = 0x7ff
)

var (
	// ErrShortHeader reports input shorter than one CRC-free ADTS header.
	ErrShortHeader = errors.New("ADTS header is shorter than 7 bytes")
	// ErrSyncWord reports a missing 12-bit ADTS syncword.
	ErrSyncWord = errors.New("invalid ADTS syncword")
)

// Header holds the fixed and variable fields of a seven-byte ADTS header.
// Field widths and meanings follow ISO/IEC 13818-7 section 6.2.1.
type Header struct {
	// MPEGID is zero for MPEG-4 and one for MPEG-2.
	MPEGID uint8
	// Profile is the two-bit ADTS profile value; AAC-LC is 1 (AOT minus one).
	Profile                uint8
	SamplingFrequencyIndex uint8
	ChannelConfiguration   uint8
	FrameLength            int
	BufferFullness         uint16
	NumRawDataBlocks       uint8
	// CRCPresent mirrors protection_absent == 0: a 16-bit CRC follows the
	// header. Build always emits CRC-free headers.
	CRCPresent bool
}

// PayloadOffset returns the offset of the raw data blocks within the frame:
// the header plus the optional CRC.
func (h Header) PayloadOffset() int {
	if h.CRCPresent {
		return HeaderSize + CRCSize
	}
	return HeaderSize
}

// Build serializes a CRC-free ADTS fixed_header() and variable_header() —
// ISO/IEC 13818-7 section 6.2.1. protection_absent is always one.
func Build(h Header) ([]byte, error) {
	if err := validate(h); err != nil {
		return nil, err
	}

	buf := make([]byte, HeaderSize)
	buf[0] = 0xff
	buf[1] = 0xf1 | h.MPEGID<<3 // syncword, ID, layer=0, protection_absent=1
	buf[2] = h.Profile<<6 | h.SamplingFrequencyIndex<<2 | h.ChannelConfiguration>>2
	buf[3] = (h.ChannelConfiguration&0x3)<<6 | byte(h.FrameLength>>11)
	buf[4] = byte(h.FrameLength >> 3)
	buf[5] = byte(h.FrameLength&0x7)<<5 | byte(h.BufferFullness>>6)
	buf[6] = byte(h.BufferFullness&0x3f)<<2 | h.NumRawDataBlocks
	return buf, nil
}

// Build serializes h as a CRC-free ADTS header.
func (h Header) Build() ([]byte, error) {
	return Build(h)
}

// Parse reads an ADTS fixed_header() and variable_header() —
// ISO/IEC 13818-7 section 6.2.1.
func Parse(buf []byte) (Header, error) {
	if len(buf) < HeaderSize {
		return Header{}, ErrShortHeader
	}
	if buf[0] != 0xff || buf[1]&0xf6 != 0xf0 {
		return Header{}, ErrSyncWord
	}

	h := Header{
		CRCPresent:             buf[1]&1 == 0,
		MPEGID:                 buf[1] >> 3 & 1,
		Profile:                buf[2] >> 6,
		SamplingFrequencyIndex: buf[2] >> 2 & 0xf,
		ChannelConfiguration:   (buf[2]&1)<<2 | buf[3]>>6,
		FrameLength:            int(buf[3]&0x3)<<11 | int(buf[4])<<3 | int(buf[5]>>5),
		BufferFullness:         uint16(buf[5]&0x1f)<<6 | uint16(buf[6]>>2),
		NumRawDataBlocks:       buf[6] & 0x3,
	}
	if err := validate(h); err != nil {
		return Header{}, err
	}
	if h.FrameLength < h.PayloadOffset() {
		return Header{}, fmt.Errorf("ADTS frame length %d is shorter than its header (%d bytes)", h.FrameLength, h.PayloadOffset())
	}
	return h, nil
}

func validate(h Header) error {
	if h.MPEGID > 1 {
		return fmt.Errorf("ADTS MPEG ID %d does not fit in 1 bit", h.MPEGID)
	}
	if h.Profile > 3 {
		return fmt.Errorf("ADTS profile %d does not fit in 2 bits", h.Profile)
	}
	if h.SamplingFrequencyIndex > 12 {
		return fmt.Errorf("unsupported ADTS sampling_frequency_index %d", h.SamplingFrequencyIndex)
	}
	if h.ChannelConfiguration > 7 {
		return fmt.Errorf("ADTS channel_configuration %d does not fit in 3 bits", h.ChannelConfiguration)
	}
	if h.FrameLength < HeaderSize || h.FrameLength > MaxFrameLength {
		return fmt.Errorf("ADTS frame length %d is outside [%d, %d]", h.FrameLength, HeaderSize, MaxFrameLength)
	}
	if h.BufferFullness > VariableBitRateBufferFullness {
		return fmt.Errorf("ADTS buffer fullness %d does not fit in 11 bits", h.BufferFullness)
	}
	if h.NumRawDataBlocks > 3 {
		return fmt.Errorf("ADTS raw data block count %d does not fit in 2 bits", h.NumRawDataBlocks)
	}
	return nil
}

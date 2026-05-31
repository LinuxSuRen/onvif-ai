// Package audio provides G.711 (PCMA/PCMU) encode/decode and PCM utilities.
package audio

import (
	"encoding/binary"
	"fmt"
)

// G711Law specifies G.711 companding law.
type G711Law int

const (
	// G711ALaw is G.711 A-law encoding.
	G711ALaw G711Law = iota
	// G711MuLaw is G.711 µ-law encoding.
	G711MuLaw
)

const (
	// PCM16kSampleRate is the sample rate used for browser capture (16kHz mono).
	PCM16kSampleRate = 16000
	// G711SampleRate is the standard G.711 sample rate.
	G711SampleRate = 8000
	// PCM24kSampleRate is used for some LLM APIs.
	PCM24kSampleRate = 24000
)

// EncodePCMToG711 encodes 16-bit linear PCM samples to G.711.
// Input: PCM 16-bit signed little-endian samples at 8kHz.
// Output: G.711 encoded bytes (one byte per sample).
func EncodePCMToG711(pcm []byte, law G711Law) ([]byte, error) {
	if len(pcm)%2 != 0 {
		return nil, fmt.Errorf("PCM data must be 16-bit (even byte length), got %d", len(pcm))
	}

	sampleCount := len(pcm) / 2
	result := make([]byte, sampleCount)

	for i := 0; i < sampleCount; i++ {
		sample := int16(binary.LittleEndian.Uint16(pcm[i*2 : i*2+2]))
		switch law {
		case G711ALaw:
			result[i] = linearToALaw(sample)
		case G711MuLaw:
			result[i] = linearToMuLaw(sample)
		}
	}
	return result, nil
}

// DecodeG711ToPCM decodes G.711 encoded bytes to 16-bit linear PCM.
// Output: PCM 16-bit signed little-endian samples at 8kHz.
func DecodeG711ToPCM(g711 []byte, law G711Law) []byte {
	result := make([]byte, len(g711)*2)
	for i, b := range g711 {
		var sample int16
		switch law {
		case G711ALaw:
			sample = aLawToLinear(b)
		case G711MuLaw:
			sample = muLawToLinear(b)
		}
		binary.LittleEndian.PutUint16(result[i*2:i*2+2], uint16(sample))
	}
	return result
}

func linearToMuLaw(sample int16) byte {
	const bias int16 = 0x84
	sign := byte(0)
	if sample < 0 {
		sign = 0x80
		sample = -sample
	} else if sample == 0 {
		return 0xFF
	}

	magnitude := int32(sample) + int32(bias)
	if magnitude > 0x7FFF {
		magnitude = 0x7FFF
	}

	exponent := byte(7)
	for expMask := int32(0x4000); (magnitude&expMask) == 0 && exponent > 0; expMask >>= 1 {
		exponent--
	}

	mantissa := byte(magnitude>>(exponent+3)) & 0x0F
	return ^(sign | (exponent << 4) | mantissa)
}

func muLawToLinear(muLaw byte) int16 {
	muLaw = ^muLaw
	sign := int16(muLaw & 0x80)
	exponent := (muLaw >> 4) & 0x07
	mantissa := int16(muLaw & 0x0F)

	sample := (mantissa << 3) + 0x84
	sample <<= exponent
	sample -= 0x84

	if sign != 0 {
		return -sample
	}
	return sample
}

// linearToALaw converts a 16-bit linear PCM sample to G.711 A-law.
func linearToALaw(sample int16) byte {
	const (
		aLawClip = 32635
	)

	mask := sample >> 15
	magnitude := int(sample)
	if magnitude < 0 {
		magnitude = -magnitude
	}
	if magnitude > aLawClip {
		magnitude = aLawClip
	}

	var exponent byte
	for expMask := 0x4000; (magnitude&expMask) == 0 && exponent < 15; expMask >>= 1 {
		exponent++
	}

	mantissa := byte(magnitude>>((exponent)+3)) & 0x0F
	alaw := byte(exponent)<<4 | mantissa
	alaw ^= byte(mask & 0x80)
	return alaw ^ 0x55
}

// aLawToLinear converts a G.711 A-law byte to a 16-bit linear PCM sample.
func aLawToLinear(aLaw byte) int16 {
	aLaw ^= 0x55
	t := int16(aLaw & 0x0F) << 4
	seg := (aLaw & 0x70) >> 4
	switch seg {
	case 0:
		t += 8
	case 1:
		t += 0x108
	default:
		t += 0x108
		t <<= seg - 1
	}
	if aLaw&0x80 != 0 {
		return t
	}
	return -t
}

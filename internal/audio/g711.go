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

// aLawSegEnd 是 A-law 各分段的右端点（13 位线性域）。
var aLawSegEnd = [8]int16{0x1F, 0x3F, 0x7F, 0xFF, 0x1FF, 0x3FF, 0x7FF, 0xFFF}

// linearToALaw converts a 16-bit linear PCM sample to a standard G.711 A-law
// byte (ITU-T G.711, equivalent to the classic Sun/ccitt reference codec).
// The previous hand-rolled encoder produced non-standard bytes (wrong segment
// math and inverted sign), which decoded to garbage on both the browser
// playback path and on camera speakers receiving our backchannel audio.
func linearToALaw(sample int16) byte {
	pcm := sample >> 3 // 16-bit linear → 13-bit
	var mask byte
	if pcm >= 0 {
		mask = 0xD5 // A-law 存反相符号位：正数置 1
	} else {
		mask = 0x55
		pcm = -pcm - 1
	}

	seg := 8
	for i, end := range aLawSegEnd {
		if pcm <= end {
			seg = i
			break
		}
	}
	if seg >= 8 {
		return 0x7F ^ mask // 超出编码范围，返回最大值
	}

	aval := byte(seg) << 4
	if seg < 2 {
		// 段 0/1 步长为 1（13 位域），两段共用 >>1 提取
		aval |= byte(pcm>>1) & 0x0F
	} else {
		// 段 seg 覆盖 [2^(seg+4), 2^(seg+5))（13 位域），
		// pcm>>seg ∈ [16, 32)，& 0xF 即减去段基得到段内步数
		aval |= byte(pcm>>seg) & 0x0F
	}
	return aval ^ mask
}

// aLawToLinear converts a G.711 A-law byte to a 16-bit linear PCM sample.
func aLawToLinear(aLaw byte) int16 {
	aLaw ^= 0x55
	t := int16(aLaw&0x0F) << 4
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

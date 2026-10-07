// Package coder implements the quantization and entropy-coding layer shared
// by the AAC encoder and decoder: scalefactor and spectral Huffman codes
// (ISO/IEC 14496-3 §4.6.3) and the inverse quantizer (§4.6.2).
package coder

import (
	"fmt"
	"sync"

	"github.com/arabian9ts/aac-go/internal/bits"
	"github.com/arabian9ts/aac-go/internal/tables"
)

// prefixDecoder decodes one codeword of an arbitrary prefix code by
// extending the accumulated code one bit at a time and probing a
// (length, code) → symbol map. Codeword lengths are at most 19 bits, so
// decoding costs at most 19 map probes — plenty fast for this codec while
// staying obviously correct.
type prefixDecoder struct {
	symbols map[uint64]int
	maxLen  int
}

func newPrefixDecoder(codes []uint32, lengths []uint8) *prefixDecoder {
	d := &prefixDecoder{symbols: make(map[uint64]int, len(codes))}
	for i := range codes {
		l := int(lengths[i])
		d.symbols[uint64(l)<<32|uint64(codes[i])] = i
		if l > d.maxLen {
			d.maxLen = l
		}
	}
	return d
}

func (d *prefixDecoder) decode(r *bits.BitReader) (int, error) {
	var code uint64
	for l := 1; l <= d.maxLen; l++ {
		bit, err := r.ReadBits(1)
		if err != nil {
			return 0, err
		}
		code = code<<1 | bit
		if symbol, ok := d.symbols[uint64(l)<<32|code]; ok {
			return symbol, nil
		}
	}
	return 0, fmt.Errorf("invalid Huffman codeword 0b%b", code)
}

var (
	scalefactorDecoder = sync.OnceValue(func() *prefixDecoder {
		return newPrefixDecoder(tables.ScalefactorCodes[:], tables.ScalefactorBits[:])
	})
	spectrumDecoders = sync.OnceValue(func() [11]*prefixDecoder {
		var out [11]*prefixDecoder
		for i, cb := range tables.SpectrumCodebooks {
			codes := make([]uint32, len(cb.Codes))
			for j, c := range cb.Codes {
				codes[j] = uint32(c)
			}
			out[i] = newPrefixDecoder(codes, cb.Bits)
		}
		return out
	})
)

// DecodeScalefactorDelta reads one hcod_sf codeword and returns the DPCM
// scalefactor delta in [-60, 60] — ISO/IEC 14496-3 Table 4.A.1.
func DecodeScalefactorDelta(r *bits.BitReader) (int, error) {
	index, err := scalefactorDecoder().decode(r)
	if err != nil {
		return 0, fmt.Errorf("scalefactor: %w", err)
	}
	return index - 60, nil
}

// DecodeSpectral reads Huffman-coded quantized coefficients for one
// scalefactor band using spectrum codebook book (1..11), filling out
// completely — ISO/IEC 14496-3 §4.6.3.2 spectral_data(). len(out) must be a
// multiple of the codebook dimension (SFB widths always are).
func DecodeSpectral(r *bits.BitReader, book int, out []int32) error {
	if book < 1 || book > 11 {
		return fmt.Errorf("spectral data uses invalid codebook %d", book)
	}
	cb := &tables.SpectrumCodebooks[book-1]
	if len(out)%cb.Dim != 0 {
		return fmt.Errorf("band width %d is not a multiple of codebook dimension %d", len(out), cb.Dim)
	}
	dec := spectrumDecoders()[book-1]

	vals := make([]int32, cb.Dim)
	for i := 0; i < len(out); i += cb.Dim {
		index, err := dec.decode(r)
		if err != nil {
			return fmt.Errorf("codebook %d: %w", book, err)
		}
		unpackTuple(cb, index, vals)

		// Unsigned books transmit one sign bit per nonzero value, in order,
		// directly after the codeword (1 = negative).
		if !cb.Signed {
			for j, v := range vals {
				if v == 0 {
					continue
				}
				negative, err := r.ReadBit()
				if err != nil {
					return err
				}
				if negative {
					vals[j] = -v
				}
			}
		}

		// In the escape book a magnitude of 16 marks an escape sequence:
		// N ones, a zero, then an (N+4)-bit word; magnitude = 2^(N+4) + word.
		if book == 11 {
			for j, v := range vals {
				if v != 16 && v != -16 {
					continue
				}
				magnitude, err := readEscape(r)
				if err != nil {
					return err
				}
				if v < 0 {
					magnitude = -magnitude
				}
				vals[j] = magnitude
			}
		}
		copy(out[i:], vals)
	}
	return nil
}

// unpackTuple converts a codebook symbol index back into Dim quantized
// values, inverting the index formulas documented in tables.SpectrumCodebook.
func unpackTuple(cb *tables.SpectrumCodebook, index int, vals []int32) {
	switch {
	case cb.Dim == 4 && cb.Signed:
		vals[0] = int32(index/27%3) - 1
		vals[1] = int32(index/9%3) - 1
		vals[2] = int32(index/3%3) - 1
		vals[3] = int32(index%3) - 1
	case cb.Dim == 4:
		vals[0] = int32(index / 27 % 3)
		vals[1] = int32(index / 9 % 3)
		vals[2] = int32(index / 3 % 3)
		vals[3] = int32(index % 3)
	case cb.Signed:
		vals[0] = int32(index/9) - 4
		vals[1] = int32(index%9) - 4
	default:
		mod := cb.LAV + 1
		vals[0] = int32(index / mod)
		vals[1] = int32(index % mod)
	}
}

// readEscape reads one escape_sequence — ISO/IEC 14496-3 §4.6.3.3.
func readEscape(r *bits.BitReader) (int32, error) {
	prefix := 0
	for {
		one, err := r.ReadBit()
		if err != nil {
			return 0, err
		}
		if !one {
			break
		}
		prefix++
		if prefix > 16 {
			return 0, fmt.Errorf("escape prefix exceeds 16 ones")
		}
	}
	wordLen := prefix + 4
	word, err := r.ReadBits(wordLen)
	if err != nil {
		return 0, err
	}
	return int32(uint32(1)<<wordLen + uint32(word)), nil
}

package coder

import (
	"fmt"

	"github.com/arabian9ts/aac-go/internal/bits"
	"github.com/arabian9ts/aac-go/internal/tables"
)

// EncodeScalefactorDelta writes one hcod_sf codeword for a DPCM scalefactor
// delta in [-60, 60] — ISO/IEC 14496-3 Table 4.A.1.
func EncodeScalefactorDelta(w *bits.BitWriter, delta int) error {
	if delta < -60 || delta > 60 {
		return fmt.Errorf("scalefactor delta %d outside [-60, 60]", delta)
	}
	index := delta + 60
	return w.WriteBits(uint64(tables.ScalefactorCodes[index]), int(tables.ScalefactorBits[index]))
}

// FitsCodebook reports whether every quantized value can be coded with
// spectrum codebook book (1..11). Book 11 fits any magnitude up to the
// escape limit 8191 (ISO/IEC 14496-3 §4.6.3.3).
func FitsCodebook(book int, q []int32) bool {
	cb := &tables.SpectrumCodebooks[book-1]
	for _, v := range q {
		if v < 0 {
			v = -v
		}
		switch {
		case book == 11:
			if v > 8191 {
				return false
			}
		case int(v) > cb.LAV:
			return false
		}
	}
	return true
}

// SpectralBits returns the exact number of bits EncodeSpectral would write
// for q using codebook book, without writing them. The caller must ensure
// FitsCodebook; len(q) must be a multiple of the codebook dimension.
func SpectralBits(book int, q []int32) (int, error) {
	return spectralWalk(nil, book, q)
}

// EncodeSpectral Huffman-codes one scalefactor band's quantized values with
// spectrum codebook book — the inverse of DecodeSpectral (ISO/IEC 14496-3
// Table 4.50): codeword, then sign bits for unsigned books, then escape
// sequences for |v| ≥ 16 in book 11.
func EncodeSpectral(w *bits.BitWriter, book int, q []int32) error {
	_, err := spectralWalk(w, book, q)
	return err
}

// spectralWalk implements both EncodeSpectral (w != nil) and SpectralBits
// (w == nil) so the cost estimate can never drift from the real encoding.
func spectralWalk(w *bits.BitWriter, book int, q []int32) (int, error) {
	if book < 1 || book > 11 {
		return 0, fmt.Errorf("invalid spectrum codebook %d", book)
	}
	cb := &tables.SpectrumCodebooks[book-1]
	if len(q)%cb.Dim != 0 {
		return 0, fmt.Errorf("band width %d is not a multiple of codebook dimension %d", len(q), cb.Dim)
	}

	total := 0
	for i := 0; i < len(q); i += cb.Dim {
		tuple := q[i : i+cb.Dim]
		index, err := packTuple(cb, book, tuple)
		if err != nil {
			return 0, err
		}
		total += int(cb.Bits[index])
		if w != nil {
			if err := w.WriteBits(uint64(cb.Codes[index]), int(cb.Bits[index])); err != nil {
				return 0, err
			}
		}

		if !cb.Signed {
			for _, v := range tuple {
				if v == 0 {
					continue
				}
				total++
				if w != nil {
					if err := w.WriteBit(v < 0); err != nil {
						return 0, err
					}
				}
			}
		}
		if book == 11 {
			for _, v := range tuple {
				if abs32(v) < 16 {
					continue
				}
				n, err := escapeBits(w, abs32(v))
				if err != nil {
					return 0, err
				}
				total += n
			}
		}
	}
	return total, nil
}

// packTuple maps Dim quantized values to the codebook symbol index — the
// inverse of unpackTuple. In the escape book, magnitudes ≥ 16 are coded as
// the escape marker 16.
func packTuple(cb *tables.SpectrumCodebook, book int, tuple []int32) (int, error) {
	clip := func(v int32) (int32, error) {
		if cb.Signed {
			if v < int32(-cb.LAV) || v > int32(cb.LAV) {
				return 0, fmt.Errorf("value %d outside signed codebook range ±%d", v, cb.LAV)
			}
			return v, nil
		}
		m := abs32(v)
		if book == 11 && m >= 16 {
			return 16, nil
		}
		if int(m) > cb.LAV {
			return 0, fmt.Errorf("magnitude %d outside codebook LAV %d", m, cb.LAV)
		}
		return m, nil
	}

	index := 0
	switch {
	case cb.Dim == 4 && cb.Signed:
		for _, v := range tuple {
			c, err := clip(v)
			if err != nil {
				return 0, err
			}
			index = index*3 + int(c+1)
		}
	case cb.Dim == 4:
		for _, v := range tuple {
			c, err := clip(v)
			if err != nil {
				return 0, err
			}
			index = index*3 + int(c)
		}
	case cb.Signed:
		for _, v := range tuple {
			c, err := clip(v)
			if err != nil {
				return 0, err
			}
			index = index*9 + int(c+4)
		}
	default:
		for _, v := range tuple {
			c, err := clip(v)
			if err != nil {
				return 0, err
			}
			index = index*(cb.LAV+1) + int(c)
		}
	}
	return index, nil
}

// escapeBits writes (or, with w == nil, counts) one escape_sequence for
// magnitude in [16, 8191] — ISO/IEC 14496-3 §4.6.3.3.
func escapeBits(w *bits.BitWriter, magnitude int32) (int, error) {
	if magnitude < 16 || magnitude > 8191 {
		return 0, fmt.Errorf("escape magnitude %d outside [16, 8191]", magnitude)
	}
	// Find the word length: magnitude = 2^wordLen + word with word < 2^wordLen.
	wordLen := 4
	for magnitude >= 1<<(wordLen+1) {
		wordLen++
	}
	prefix := wordLen - 4 // number of leading ones
	word := uint64(magnitude) - 1<<wordLen

	total := prefix + 1 + wordLen
	if w == nil {
		return total, nil
	}
	for range prefix {
		if err := w.WriteBit(true); err != nil {
			return 0, err
		}
	}
	if err := w.WriteBit(false); err != nil {
		return 0, err
	}
	if err := w.WriteBits(word, wordLen); err != nil {
		return 0, err
	}
	return total, nil
}

func abs32(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}

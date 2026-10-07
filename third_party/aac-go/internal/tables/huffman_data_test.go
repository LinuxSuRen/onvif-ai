package tables

import (
	"testing"
)

// codebookName returns a stable label for error messages.
func codebookName(i int) string {
	return []string{"1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11(ESC)"}[i]
}

// TestScalefactorTableIsCompletePrefixCode validates the transcription of
// ISO/IEC 14496-3 Table 4.A.1: every codeword fits its declared length, the
// code is prefix-free, and the Kraft sum is exactly 1 (a complete code).
func TestScalefactorTableIsCompletePrefixCode(t *testing.T) {
	codes := make([]uint32, len(ScalefactorCodes))
	lengths := make([]int, len(ScalefactorBits))
	for i := range ScalefactorCodes {
		codes[i] = ScalefactorCodes[i]
		lengths[i] = int(ScalefactorBits[i])
	}
	checkPrefixCode(t, "scalefactor", codes, lengths, true)
}

// TestSpectrumCodebooksArePrefixCodes validates the transcriptions of
// ISO/IEC 14496-3 Tables 4.A.2-4.A.12.
func TestSpectrumCodebooksArePrefixCodes(t *testing.T) {
	for i, cb := range SpectrumCodebooks {
		if len(cb.Codes) != len(cb.Bits) {
			t.Fatalf("codebook %s: %d codes but %d lengths", codebookName(i), len(cb.Codes), len(cb.Bits))
		}
		wantEntries := 0
		switch {
		case cb.Dim == 4 && cb.Signed:
			wantEntries = 3 * 3 * 3 * 3 // values in [-1,1]
		case cb.Dim == 4 && !cb.Signed:
			wantEntries = 3 * 3 * 3 * 3 // values in [0,2]
		case cb.Dim == 2 && cb.Signed:
			wantEntries = 9 * 9 // values in [-4,4]
		default:
			wantEntries = (cb.LAV + 1) * (cb.LAV + 1)
		}
		if len(cb.Codes) != wantEntries {
			t.Fatalf("codebook %s: %d entries, want %d", codebookName(i), len(cb.Codes), wantEntries)
		}
		codes := make([]uint32, len(cb.Codes))
		lengths := make([]int, len(cb.Bits))
		for j := range cb.Codes {
			codes[j] = uint32(cb.Codes[j])
			lengths[j] = int(cb.Bits[j])
		}
		checkPrefixCode(t, "spectrum "+codebookName(i), codes, lengths, true)
	}
}

// checkPrefixCode asserts codewords fit their lengths, no codeword is a
// prefix of another, and (if complete) the Kraft sum equals 1. It also
// decodes every codeword bit-by-bit with an independent brute-force decoder
// to prove the (code, length) pairs are unambiguous.
func checkPrefixCode(t *testing.T, name string, codes []uint32, lengths []int, wantComplete bool) {
	t.Helper()

	// Kraft sum in exact integer arithmetic: sum of 2^(maxLen-len).
	maxLen := 0
	for i, l := range lengths {
		if l <= 0 || l > 32 {
			t.Fatalf("%s[%d]: invalid length %d", name, i, l)
		}
		if codes[i] >= 1<<uint(l) {
			t.Fatalf("%s[%d]: codeword 0x%x does not fit in %d bits", name, i, codes[i], l)
		}
		if l > maxLen {
			maxLen = l
		}
	}
	var kraft uint64
	for _, l := range lengths {
		kraft += 1 << uint(maxLen-l)
	}
	if wantComplete && kraft != 1<<uint(maxLen) {
		t.Errorf("%s: Kraft sum %d/%d, want complete code", name, kraft, uint64(1)<<uint(maxLen))
	}

	// Prefix-freedom: no codeword may be a prefix of a longer one.
	for i := range codes {
		for j := range codes {
			if i == j || lengths[i] > lengths[j] {
				continue
			}
			if lengths[i] == lengths[j] {
				if codes[i] == codes[j] && i < j {
					t.Fatalf("%s: duplicate codeword 0x%x (entries %d and %d)", name, codes[i], i, j)
				}
				continue
			}
			if codes[j]>>uint(lengths[j]-lengths[i]) == codes[i] {
				t.Fatalf("%s: entry %d (0x%x/%d) is a prefix of entry %d (0x%x/%d)",
					name, i, codes[i], lengths[i], j, codes[j], lengths[j])
			}
		}
	}

	// Exhaustive decode roundtrip with an independent bit-by-bit decoder.
	for i := range codes {
		sym, consumed := decodeBitByBit(codes, lengths, codes[i], lengths[i])
		if sym != i || consumed != lengths[i] {
			t.Fatalf("%s: codeword %d decodes to symbol %d (consumed %d bits, want %d)",
				name, i, sym, consumed, lengths[i])
		}
	}
}

// decodeBitByBit consumes bits MSB-first from word (of bitLen bits) and
// returns the first matching symbol. Independent of any production decoder.
func decodeBitByBit(codes []uint32, lengths []int, word uint32, bitLen int) (symbol, consumed int) {
	var acc uint32
	for n := 1; n <= bitLen; n++ {
		acc = (acc << 1) | ((word >> uint(bitLen-n)) & 1)
		for i := range codes {
			if lengths[i] == n && codes[i] == acc {
				return i, n
			}
		}
	}
	return -1, 0
}

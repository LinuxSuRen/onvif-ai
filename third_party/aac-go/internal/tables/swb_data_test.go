package tables

import "testing"

// TestSWBOffsetsAreConsistent validates the generated SFB tables: strictly
// increasing boundaries starting at 0, ending at the transform size, and a
// band count matching FFmpeg's independent num_swb arrays.
func TestSWBOffsetsAreConsistent(t *testing.T) {
	check := func(name string, offsets [13][]uint16, numSWB [13]int, end uint16) {
		for idx, offs := range offsets {
			if offs == nil {
				continue
			}
			if offs[0] != 0 || offs[len(offs)-1] != end {
				t.Fatalf("%s[%d]: boundaries [%d..%d], want [0..%d]", name, idx, offs[0], offs[len(offs)-1], end)
			}
			for i := 1; i < len(offs); i++ {
				if offs[i] <= offs[i-1] {
					t.Fatalf("%s[%d]: offsets not increasing at %d (%d <= %d)", name, idx, i, offs[i], offs[i-1])
				}
			}
			if got := len(offs) - 1; got != numSWB[idx] {
				t.Fatalf("%s[%d]: %d bands, num_swb says %d", name, idx, got, numSWB[idx])
			}
		}
	}
	check("LongSWBOffsets", LongSWBOffsets, NumSWBLong, 1024)
	check("ShortSWBOffsets", ShortSWBOffsets, NumSWBShort, 128)
}

// TestTNSCoefficientTables pins the shape of the TNS LPC dequantization
// tables: 2^(coef_len) entries each, all magnitudes below 1 (stable filter
// reflection coefficients).
func TestTNSCoefficientTables(t *testing.T) {
	wantLen := [4]int{8, 16, 4, 8} // index = 2*coef_compress + coef_res
	for i, table := range TNSCoefficients {
		if len(table) != wantLen[i] {
			t.Fatalf("TNSCoefficients[%d]: %d entries, want %d", i, len(table), wantLen[i])
		}
		for j, c := range table {
			if c <= -1 || c >= 1 {
				t.Fatalf("TNSCoefficients[%d][%d] = %f out of (-1, 1)", i, j, c)
			}
		}
	}
}

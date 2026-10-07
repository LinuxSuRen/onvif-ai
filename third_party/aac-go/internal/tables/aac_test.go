package tables

import "testing"

func TestSamplingFrequencyIndex(t *testing.T) {
	for wantIndex, frequency := range SamplingFrequencies {
		gotIndex, ok := SamplingFrequencyIndex(frequency)
		if !ok || gotIndex != wantIndex {
			t.Fatalf("SamplingFrequencyIndex(%d) = (%d, %t), want (%d, true)", frequency, gotIndex, ok, wantIndex)
		}
		gotFrequency, ok := SamplingFrequency(wantIndex)
		if !ok || gotFrequency != frequency {
			t.Fatalf("SamplingFrequency(%d) = (%d, %t), want (%d, true)", wantIndex, gotFrequency, ok, frequency)
		}
	}
	if _, ok := SamplingFrequencyIndex(12345); ok {
		t.Fatal("unsupported frequency was accepted")
	}
	if _, ok := SamplingFrequency(len(SamplingFrequencies)); ok {
		t.Fatal("out-of-range index was accepted")
	}
}

func TestLongWindowSFBOffsets(t *testing.T) {
	for _, index := range []int{3, 4} {
		offsets, ok := LongWindowSFBOffsets(index)
		if !ok {
			t.Fatalf("LongWindowSFBOffsets(%d) was not found", index)
		}
		if got, want := len(offsets), 50; got != want {
			t.Fatalf("index %d: got %d boundaries, want %d", index, got, want)
		}
		if offsets[0] != 0 || offsets[len(offsets)-1] != 1024 {
			t.Fatalf("index %d: invalid endpoints %d..%d", index, offsets[0], offsets[len(offsets)-1])
		}
		for i := 1; i < len(offsets); i++ {
			if offsets[i] <= offsets[i-1] || offsets[i]%4 != 0 {
				t.Fatalf("index %d: invalid boundary at %d: %v", index, i, offsets)
			}
		}
	}
	// All 13 sampling_frequency_index values are covered since M5; only
	// out-of-range indices are rejected.
	if _, ok := LongWindowSFBOffsets(13); ok {
		t.Fatal("out-of-range SFB table was accepted")
	}
	if _, ok := ShortWindowSFBOffsets(-1); ok {
		t.Fatal("negative SFB index was accepted")
	}
}

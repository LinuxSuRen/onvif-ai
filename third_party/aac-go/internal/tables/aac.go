// Package tables contains data tables shared by the AAC codec stages.
package tables

// SamplingFrequencies maps sampling_frequency_index to Hz — ISO/IEC 14496-3
// Table 1.16. Index 15 is the explicit-frequency escape value and is omitted.
var SamplingFrequencies = [...]int{
	96000, 88200, 64000, 48000, 44100, 32000, 24000,
	22050, 16000, 12000, 11025, 8000, 7350,
}

// SamplingFrequencyIndex returns the MPEG-4 sampling_frequency_index for hertz.
func SamplingFrequencyIndex(hertz int) (int, bool) {
	for index, frequency := range SamplingFrequencies {
		if frequency == hertz {
			return index, true
		}
	}
	return 0, false
}

// SamplingFrequency returns the frequency associated with index.
func SamplingFrequency(index int) (int, bool) {
	if index < 0 || index >= len(SamplingFrequencies) {
		return 0, false
	}
	return SamplingFrequencies[index], true
}

// LongWindowSFBOffsets returns the 1024-sample long-window SFB boundaries for
// sampling_frequency_index (generated data in swb_data.go). The returned
// slice must not be modified.
func LongWindowSFBOffsets(index int) ([]uint16, bool) {
	if index < 0 || index >= len(LongSWBOffsets) || LongSWBOffsets[index] == nil {
		return nil, false
	}
	return LongSWBOffsets[index], true
}

// ShortWindowSFBOffsets returns the 128-sample short-window SFB boundaries
// for sampling_frequency_index (generated data in swb_data.go). The returned
// slice must not be modified.
func ShortWindowSFBOffsets(index int) ([]uint16, bool) {
	if index < 0 || index >= len(ShortSWBOffsets) || ShortSWBOffsets[index] == nil {
		return nil, false
	}
	return ShortSWBOffsets[index], true
}

package aac

import (
	"fmt"
	"math"
	"math/rand"

	"github.com/arabian9ts/aac-go/internal/bits"
	"github.com/arabian9ts/aac-go/internal/tables"
)

type tnsData struct {
	windows []tnsWindow
}

type tnsWindow struct {
	filters []tnsFilter
}

type tnsFilter struct {
	length       int
	direction    bool
	coefficients []float64
	// coefficientCodes holds the uncompressed 4-bit table indices used by
	// the encoder. Parsed streams need only coefficients; keeping the codes
	// here prevents a second, potentially tie-breaking, nearest lookup while
	// writing tns_data().
	coefficientCodes []uint8
}

// parseTNS reads tns_data() — ISO/IEC 14496-3 Table 4.48. Long windows
// carry up to 3 filters of order ≤ 12 per window; EIGHT_SHORT carries up
// to 1 filter of order ≤ 7 per short window, with narrower length/order
// fields. Coefficients are table-coded in (coef_res + 3 − coef_compress)
// bits; see tables.TNSCoefficients for the dequantization mapping.
func parseTNS(r *bits.BitReader, windowSequence int) (*tnsData, error) {
	short := windowSequence == eightShortSequence
	numWindows := 1
	maxOrder := 12 // AAC-LC long-window limit.
	if short {
		numWindows = 8
		maxOrder = 7
	}

	tns := &tnsData{windows: make([]tnsWindow, numWindows)}
	read := func(name string, width int) (int, error) {
		value, err := r.ReadBits(width)
		if err != nil {
			return 0, fmt.Errorf("aac: tns %s: %w", name, err)
		}
		return int(value), nil
	}

	for w := range numWindows {
		nFiltWidth := 2
		if short {
			nFiltWidth = 1
		}
		nFilt, err := read("n_filt", nFiltWidth)
		if err != nil {
			return nil, err
		}
		if nFilt == 0 {
			continue
		}

		coefRes, err := read("coef_res", 1)
		if err != nil {
			return nil, err
		}
		tns.windows[w].filters = make([]tnsFilter, nFilt)
		for filt := range nFilt {
			lengthWidth, orderWidth := 6, 5
			if short {
				lengthWidth, orderWidth = 4, 3
			}
			length, err := read("length", lengthWidth)
			if err != nil {
				return nil, err
			}
			order, err := read("order", orderWidth)
			if err != nil {
				return nil, err
			}
			if order > maxOrder {
				return nil, fmt.Errorf("aac: TNS filter order %d exceeds AAC-LC maximum %d", order, maxOrder)
			}

			filter := &tns.windows[w].filters[filt]
			filter.length = length
			if order == 0 {
				continue
			}
			direction, err := read("direction", 1)
			if err != nil {
				return nil, err
			}
			coefCompress, err := read("coef_compress", 1)
			if err != nil {
				return nil, err
			}
			filter.direction = direction != 0
			filter.coefficients = make([]float64, order)
			coefWidth := coefRes + 3 - coefCompress
			coefTable := tables.TNSCoefficients[2*coefCompress+coefRes]
			for i := range order {
				code, err := read("coef", coefWidth)
				if err != nil {
					return nil, err
				}
				filter.coefficients[i] = coefTable[code]
			}
		}
	}
	return tns, nil
}

const tnsPredictionGainThreshold = 1.4

// analyzeTNS derives and applies one AAC-LC TNS analysis filter per window
// spec contains one 1024-line long spectrum or eight
// consecutive 128-line short spectra. The function mutates spec with the MA
// analysis filter and returns the Table 4.48 data needed to invert it.
//
// Reflection coefficients come from autocorrelation and Levinson-Durbin,
// then use the nearest entry in the uncompressed 4-bit coefficient table.
// Filters whose unquantized prediction gain does not exceed 1.4 are omitted.
func analyzeTNS(spec []float64, offsets []uint16, maxSFB int, shortWindow bool) *tnsData {
	numWindows, maxOrder := 1, 12
	if shortWindow {
		numWindows, maxOrder = 8, 7
	}
	tns := &tnsData{windows: make([]tnsWindow, numWindows)}
	if len(offsets) < 2 || maxSFB <= 0 {
		return tns
	}

	numBands := len(offsets) - 1
	maxSFB = min(maxSFB, numBands)
	windowSize := int(offsets[numBands])
	end := int(offsets[maxSFB])
	if windowSize <= 0 || end <= 0 {
		return tns
	}

	for w := range numWindows {
		base := w * windowSize
		if base+end > len(spec) {
			break
		}
		transmitted, gain := tnsLevinson(spec[base:base+end], maxOrder)
		if gain <= tnsPredictionGainThreshold || len(transmitted) == 0 {
			continue
		}

		coefficients, codes := quantizeTNSCoefficients(transmitted)
		// Trailing zero reflection coefficients add syntax bits but do not
		// change the filter order below them.
		for len(coefficients) > 0 && codes[len(codes)-1] == 0 {
			coefficients = coefficients[:len(coefficients)-1]
			codes = codes[:len(codes)-1]
		}
		if len(coefficients) == 0 {
			continue
		}

		tns.windows[w].filters = []tnsFilter{{
			// TNS lengths are counted down from num_swb. Covering the
			// encoder's active [0,maxSFB) range therefore requires numBands;
			// decoder-side maxSFB clamping supplies the upper boundary.
			length:           numBands,
			coefficients:     coefficients,
			coefficientCodes: codes,
		}}
		applyTNSAnalysis(spec, tns, w, offsets, maxSFB)
	}
	return tns
}

// tnsLevinson returns reflection coefficients in AAC's transmitted sign
// convention and the prediction gain. The internal Levinson coefficient k
// is the MA predictor's reflection coefficient; AAC transmits −k to match
// the sign convention tnsDirectForm reverses during decoding.
func tnsLevinson(samples []float64, order int) ([]float64, float64) {
	order = min(order, len(samples)-1)
	if order <= 0 {
		return nil, 1
	}

	autocorrelation := make([]float64, order+1)
	for lag := range autocorrelation {
		for i := lag; i < len(samples); i++ {
			autocorrelation[lag] += samples[i] * samples[i-lag]
		}
	}
	energy := autocorrelation[0]
	if energy <= 0 || math.IsNaN(energy) || math.IsInf(energy, 0) {
		return nil, 1
	}
	// Normalization makes the recursion insensitive to MDCT amplitude and
	// keeps intermediate values well away from overflow.
	for i := range autocorrelation {
		autocorrelation[i] /= energy
	}

	lpc := make([]float64, order)
	transmitted := make([]float64, order)
	errorEnergy := 1.0
	actualOrder := 0
	for i := 0; i < order; i++ {
		residual := autocorrelation[i+1]
		for j := 0; j < i; j++ {
			residual += lpc[j] * autocorrelation[i-j]
		}
		if errorEnergy <= 1e-12 {
			break
		}
		reflection := -residual / errorEnergy
		if math.IsNaN(reflection) || math.IsInf(reflection, 0) {
			break
		}
		// Finite sample autocorrelation should be positive semi-definite;
		// clipping only guards roundoff at a near-unit reflection pole.
		reflection = math.Max(-0.999, math.Min(0.999, reflection))

		next := append([]float64(nil), lpc...)
		for j := 0; j < i; j++ {
			next[j] = lpc[j] + reflection*lpc[i-1-j]
		}
		next[i] = reflection
		lpc = next
		transmitted[i] = -reflection
		errorEnergy *= 1 - reflection*reflection
		actualOrder = i + 1
	}
	if actualOrder == 0 || errorEnergy <= 0 {
		return nil, 1
	}
	return transmitted[:actualOrder], 1 / errorEnergy
}

func quantizeTNSCoefficients(reflection []float64) ([]float64, []uint8) {
	table := tables.TNSCoefficients[1] // coef_res=1, coef_compress=0: 4 bits
	coefficients := make([]float64, len(reflection))
	codes := make([]uint8, len(reflection))
	for i, value := range reflection {
		best := 0
		bestDistance := math.Abs(value - table[0])
		for code := 1; code < len(table); code++ {
			if distance := math.Abs(value - table[code]); distance < bestDistance {
				best, bestDistance = code, distance
			}
		}
		coefficients[i] = table[best]
		codes[i] = uint8(best)
	}
	return coefficients, codes
}

func tnsPresent(tns *tnsData) bool {
	if tns == nil {
		return false
	}
	for _, window := range tns.windows {
		if len(window.filters) != 0 {
			return true
		}
	}
	return false
}

// writeTNSData writes tns_data() for AAC-LC — ISO/IEC 14496-3 Table 4.48.
// Encoder filters always use the full, uncompressed 4-bit coefficient table
// (coef_res=1, coef_compress=0), matching analyzeTNS.
func writeTNSData(w *bits.BitWriter, tns *tnsData, shortWindow bool) error {
	numWindows, nFiltWidth := 1, 2
	lengthWidth, orderWidth, maxOrder := 6, 5, 12
	if shortWindow {
		numWindows, nFiltWidth = 8, 1
		lengthWidth, orderWidth, maxOrder = 4, 3, 7
	}
	gotWindows := 0
	if tns != nil {
		gotWindows = len(tns.windows)
	}
	if gotWindows < numWindows {
		return fmt.Errorf("AAC TNS has %d windows, need %d", gotWindows, numWindows)
	}

	for window := range numWindows {
		filters := tns.windows[window].filters
		if len(filters) > (1<<nFiltWidth)-1 {
			return fmt.Errorf("AAC TNS window %d has %d filters", window, len(filters))
		}
		if err := w.WriteBits(uint64(len(filters)), nFiltWidth); err != nil {
			return err
		}
		if len(filters) == 0 {
			continue
		}
		if err := w.WriteBits(1, 1); err != nil { // coef_res: 4-bit codes
			return err
		}
		for _, filter := range filters {
			order := len(filter.coefficients)
			if filter.length < 0 || filter.length >= 1<<lengthWidth || order > maxOrder {
				return fmt.Errorf("AAC TNS filter length/order %d/%d does not fit", filter.length, order)
			}
			if err := w.WriteBits(uint64(filter.length), lengthWidth); err != nil {
				return err
			}
			if err := w.WriteBits(uint64(order), orderWidth); err != nil {
				return err
			}
			if order == 0 {
				continue
			}
			if err := w.WriteBits(uint64(boolBit(filter.direction)), 1); err != nil {
				return err
			}
			if err := w.WriteBits(0, 1); err != nil { // coef_compress
				return err
			}
			for i, coefficient := range filter.coefficients {
				code := nearestTNSCoefficientCode(coefficient)
				if i < len(filter.coefficientCodes) {
					code = filter.coefficientCodes[i]
				}
				if code >= 16 {
					return fmt.Errorf("AAC TNS coefficient code %d does not fit 4 bits", code)
				}
				if err := w.WriteBits(uint64(code), 4); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func nearestTNSCoefficientCode(value float64) uint8 {
	table := tables.TNSCoefficients[1]
	best := 0
	bestDistance := math.Abs(value - table[0])
	for code := 1; code < len(table); code++ {
		if distance := math.Abs(value - table[code]); distance < bestDistance {
			best, bestDistance = code, distance
		}
	}
	return uint8(best)
}

func boolBit(value bool) int {
	if value {
		return 1
	}
	return 0
}

// tnsRegion is the run of spectral coefficients one TNS filter covers, in
// filter-traversal order: (start, step) walk length count.
type tnsRegion struct {
	start, step, count int
}

// tnsFilterRegions resolves each filter of window w to its coefficient
// region. Filter lengths allocate scalefactor bands from the top band
// downward (ISO/IEC 14496-3 §4.6.9.2); the active range is clamped to
// maxSFB (the caller passes min(max_sfb, tns_max_bands)), and the
// direction flag reverses the traversal. Filters that resolve to an empty
// or out-of-bounds region are dropped.
func tnsFilterRegions(tns *tnsData, w int, offsets []uint16, maxSFB, specLen int) []struct {
	filter *tnsFilter
	region tnsRegion
} {
	if tns == nil || w < 0 || w >= len(tns.windows) || len(offsets) < 2 || maxSFB <= 0 {
		return nil
	}
	numBands := len(offsets) - 1
	maxSFB = min(maxSFB, numBands)
	windowBase := w * int(offsets[numBands])

	var out []struct {
		filter *tnsFilter
		region tnsRegion
	}
	top := numBands
	for f := range tns.windows[w].filters {
		filter := &tns.windows[w].filters[f]
		bottom := max(0, top-filter.length)
		lo := windowBase + int(offsets[min(bottom, maxSFB)])
		hi := windowBase + int(offsets[min(top, maxSFB)])
		top = bottom
		if len(filter.coefficients) == 0 || hi <= lo || hi > specLen {
			continue
		}
		region := tnsRegion{start: lo, step: 1, count: hi - lo}
		if filter.direction {
			region = tnsRegion{start: hi - 1, step: -1, count: hi - lo}
		}
		out = append(out, struct {
			filter *tnsFilter
			region tnsRegion
		}{filter, region})
	}
	return out
}

// gatherRegion copies a region's coefficients into scratch in traversal
// order; scatterRegion writes them back.
func gatherRegion(spec []float64, r tnsRegion) []float64 {
	out := make([]float64, r.count)
	for m := range out {
		out[m] = spec[r.start+m*r.step]
	}
	return out
}

func scatterRegion(spec []float64, r tnsRegion, values []float64) {
	for m, v := range values {
		spec[r.start+m*r.step] = v
	}
}

// applyTNSAnalysis runs the encoder-side FIR (moving-average) noise-shaping
// filter y[m] = x[m] + Σ a_i·x[m−i] over each filter region, so that the
// decoder's all-pole filter reconstructs the original spectrum.
func applyTNSAnalysis(spec []float64, tns *tnsData, w int, offsets []uint16, maxSFB int) {
	for _, fr := range tnsFilterRegions(tns, w, offsets, maxSFB, len(spec)) {
		a := tnsDirectForm(fr.filter.coefficients)
		x := gatherRegion(spec, fr.region)
		y := make([]float64, len(x))
		for m := range x {
			y[m] = x[m]
			for i := 1; i <= min(m, len(a)); i++ {
				y[m] += a[i-1] * x[m-i]
			}
		}
		scatterRegion(spec, fr.region, y)
	}
}

// applyTNS runs the decoder-side all-pole synthesis filter
// y[m] = x[m] − Σ a_i·y[m−i] over each filter region — ISO/IEC 14496-3
// §4.6.9.3. Short-window spectra occupy consecutive 128-coefficient
// regions; the caller iterates windows.
func applyTNS(spec []float64, tns *tnsData, w int, offsets []uint16, maxSFB int) {
	for _, fr := range tnsFilterRegions(tns, w, offsets, maxSFB, len(spec)) {
		a := tnsDirectForm(fr.filter.coefficients)
		y := gatherRegion(spec, fr.region)
		for m := range y {
			for i := 1; i <= min(m, len(a)); i++ {
				y[m] -= a[i-1] * y[m-i]
			}
		}
		scatterRegion(spec, fr.region, y)
	}
}

// tnsDirectForm converts transmitted reflection coefficients into the
// direct-form coefficients a_1..a_p of A(z) = 1 + Σ a_i z^(−i) using the
// standard lattice step-up recursion,
//
//	a^(m)_i = a^(m−1)_i + k_m·a^(m−1)_{m−i},   a^(m)_m = k_m,
//
// with k_m the negated transmitted value: the sign convention of the
// transmitted coefficients (tables.TNSCoefficients) is fixed by the
// standard's decoding process and verified against reference decoders in
// the acceptance suite.
func tnsDirectForm(transmitted []float64) []float64 {
	a := make([]float64, 0, len(transmitted))
	for _, t := range transmitted {
		k := -t
		prev := append([]float64(nil), a...)
		a = append(a, k)
		for i := 1; i <= len(prev); i++ {
			a[i-1] = prev[i-1] + k*prev[len(prev)-i]
		}
	}
	return a
}

// applyMidSide decodes one selected M/S stereo band — ISO/IEC 14496-3
// §4.6.8.1.3: the transmitted channels carry mid and side, and the
// butterfly l = m + s, r = m − s restores left/right.
func applyMidSide(left, right []float64) {
	for i := range min(len(left), len(right)) {
		mid, side := left[i], right[i]
		left[i] = mid + side
		right[i] = mid - side
	}
}

// applyIntensity reconstructs one right-channel intensity-stereo band from
// the left channel — ISO/IEC 14496-3 §4.6.8.2.3: the right channel is the
// left scaled by 2^(−is_position/4), with invert carrying the combined
// codebook (in/out of phase) and M/S-mask sign. The scaling convention is
// verified against reference decoders in the acceptance suite.
func applyIntensity(left, right []float64, position int, invert bool) {
	scale := math.Exp2(-float64(position) / 4)
	if invert {
		scale = -scale
	}
	for i := range min(len(left), len(right)) {
		right[i] = left[i] * scale
	}
}

// fillPNS synthesizes one perceptual-noise-substitution band — ISO/IEC
// 14496-3 §4.6.13: the band is filled with white noise normalized so its
// amplitude scale equals 2^(noiseEnergy/4), where noiseEnergy is the
// decoded PNS energy accumulator (see parseScalefactorData). The standard
// prescribes only the noise distribution, not the generator, so any PRNG
// is conformant; reference decoders legitimately produce different
// waveforms with the same envelope.
func fillPNS(band []float64, noiseEnergy int, rng *rand.Rand) {
	if len(band) == 0 {
		return
	}
	var energy float64
	for i := range band {
		value := 2*rng.Float64() - 1
		band[i] = value
		energy += value * value
	}
	if energy == 0 {
		band[0] = 1
		energy = 1
	}
	scale := math.Exp2(float64(noiseEnergy)/4) / math.Sqrt(energy)
	for i := range band {
		band[i] *= scale
	}
}

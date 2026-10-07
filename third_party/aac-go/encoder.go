package aac

import (
	"fmt"
	"math"

	"github.com/arabian9ts/aac-go/adts"
	"github.com/arabian9ts/aac-go/internal/bits"
	"github.com/arabian9ts/aac-go/internal/coder"
	"github.com/arabian9ts/aac-go/internal/tables"
	"github.com/arabian9ts/aac-go/internal/transform"
)

// Encoder buffers interleaved PCM and emits ADTS frames.
//
// Each 1024-sample block is transformed together with the previous block
// (50% MDCT overlap). Block switching needs one block
// of lookahead — a frame becomes LONG_START only when the *next* frame is
// known to hold a transient — so a frame is emitted when the block after
// it arrives, and Flush drains the pipeline plus the final MDCT tail.
type Encoder struct {
	config                 Config
	samplingFrequencyIndex int
	baseScalefactor        int

	mdctLong  *transform.MDCT
	mdctShort *transform.MDCT
	winLong   []float64 // sine, 2048
	winShort  []float64 // sine, 256

	pending []int16 // buffered partial interleaved input block

	// Block pipeline: frame (blockA | blockB) is encoded when the next
	// block arrives, so its window sequence can consider that block's
	// transients. Blocks are stored per channel, deinterleaved.
	blockA, blockB [][]float64
	// Attack flags, split by position: a frame's eight short windows span
	// [448, 1600) of its 2048 span, so an attack early in a block is
	// covered by the frame having that block on the RIGHT, and a late
	// attack by the frame having it on the LEFT.
	earlyB, lateB bool
	lateA         bool
	pushed        int // blocks accepted into the pipeline
	prevSequence  int // window sequence of the last emitted frame
	lastEnergy    float64

	started bool
	flushed bool
}

// NewEncoder validates config and returns an AAC-LC streaming encoder.
func NewEncoder(config Config) (*Encoder, error) {
	index, err := config.validate()
	if err != nil {
		return nil, err
	}
	e := &Encoder{
		config:                 config,
		samplingFrequencyIndex: index,
		// Quality maps linearly onto a uniform noise floor: one base
		// scalefactor shared by every band. Lower
		// scalefactor = finer quantization = higher fidelity and bitrate.
		baseScalefactor: quietestScalefactor - int(math.Round(config.Quality*qualityScalefactorRange)),
		mdctLong:        transform.New(2 * SamplesPerFrame),
		mdctShort:       transform.New(2 * SamplesPerFrame / 8),
		winLong:         transform.SineWindow(2 * SamplesPerFrame),
		winShort:        transform.SineWindow(2 * SamplesPerFrame / 8),
		pending:         make([]int16, 0, SamplesPerFrame*config.Channels),
		blockA:          zeroBlocks(config.Channels),
		blockB:          zeroBlocks(config.Channels),
	}
	return e, nil
}

func zeroBlocks(channels int) [][]float64 {
	blocks := make([][]float64, channels)
	for ch := range blocks {
		blocks[ch] = make([]float64, SamplesPerFrame)
	}
	return blocks
}

// Encode consumes interleaved PCM of any length and returns the ADTS frames
// completed by this call, concatenated. Because of the one-block lookahead,
// the first frame is returned once the second block is complete.
func (e *Encoder) Encode(pcm []int16) ([]byte, error) {
	if e.flushed {
		return nil, ErrEncoderFlushed
	}

	var out []byte
	blockSamples := SamplesPerFrame * e.config.Channels
	if len(e.pending) != 0 {
		consumed := min(len(pcm), blockSamples-len(e.pending))
		e.pending = append(e.pending, pcm[:consumed]...)
		pcm = pcm[consumed:]
		if len(e.pending) == blockSamples {
			frame, err := e.pushBlock(e.pending)
			if err != nil {
				return nil, err
			}
			out = append(out, frame...)
			e.pending = e.pending[:0]
		}
	}
	for len(pcm) >= blockSamples {
		frame, err := e.pushBlock(pcm[:blockSamples])
		if err != nil {
			return nil, err
		}
		out = append(out, frame...)
		pcm = pcm[blockSamples:]
	}
	e.pending = append(e.pending, pcm...)
	return out, nil
}

// Flush zero-pads a partial block, drains the lookahead pipeline, and
// appends one final frame covering the MDCT tail of the last block.
// Repeated calls, or Encode after Flush, return ErrEncoderFlushed.
func (e *Encoder) Flush() ([]byte, error) {
	if e.flushed {
		return nil, ErrEncoderFlushed
	}
	e.flushed = true

	var out []byte
	blockSamples := SamplesPerFrame * e.config.Channels
	if len(e.pending) != 0 {
		block := append(e.pending, make([]int16, blockSamples-len(e.pending))...)
		frame, err := e.pushBlock(block)
		if err != nil {
			return nil, err
		}
		out = append(out, frame...)
		e.pending = nil
	}
	if e.pushed == 0 {
		return out, nil
	}
	// Drain with two zero blocks: the first flushes the pipelined last
	// frame, the second emits the tail frame covering the final block's
	// MDCT overlap. Total frames = real blocks + 1, as before lookahead.
	for range 2 {
		frame, err := e.pushBlock(make([]int16, blockSamples))
		if err != nil {
			return nil, err
		}
		out = append(out, frame...)
	}
	return out, nil
}

// pushBlock accepts one complete interleaved block and, once the pipeline
// holds a successor, encodes the previous frame.
func (e *Encoder) pushBlock(block []int16) ([]byte, error) {
	e.started = true
	channels := e.config.Channels
	incoming := make([][]float64, channels)
	for ch := range channels {
		samples := make([]float64, SamplesPerFrame)
		for i := range samples {
			samples[i] = float64(block[i*channels+ch])
		}
		incoming[ch] = samples
	}
	earlyC, lateC := e.detectTransient(incoming)

	var out []byte
	if e.pushed >= 1 {
		short := e.lateA || e.earlyB   // attack inside this frame's short-window span
		nextShort := e.lateB || earlyC // ... inside the next frame's span
		frame, err := e.emitFrame(e.nextSequence(short, nextShort))
		if err != nil {
			return nil, err
		}
		out = frame
	}
	e.blockA, e.blockB = e.blockB, incoming
	e.lateA = e.lateB
	e.earlyB, e.lateB = earlyC, lateC
	e.pushed++
	return out, nil
}

// Transient detection constants: a 128-sample sub-block
// whose energy jumps attackRatio× above the running average of its
// predecessors — and above an absolute floor that keeps silence and hiss
// from triggering — marks the block as transient.
const (
	transientSubBlocks   = 8
	transientAttackRatio = 8.0
	transientEnergyFloor = 128 * 500 * 500 // sub-block energy of a ~500-amplitude signal
	transientDecayCarry  = 0.5             // weight of the previous block's last sub-blocks
)

// detectTransient reports attacks inside the incoming block, split by
// position: sub-blocks 0-4 (early: covered by the frame carrying this
// block on the right) and 4-7 (late: covered by the next frame). For CPE
// frames the window sequence is shared, so the per-channel decisions are
// ORed.
func (e *Encoder) detectTransient(blocks [][]float64) (early, late bool) {
	const sub = SamplesPerFrame / transientSubBlocks
	maxEnergy := 0.0
	for _, samples := range blocks {
		baseline := e.lastEnergy * transientDecayCarry
		count := 1.0
		for s := 0; s < transientSubBlocks; s++ {
			energy := 0.0
			for _, v := range samples[s*sub : (s+1)*sub] {
				energy += v * v
			}
			mean := baseline / count
			if energy > transientEnergyFloor && energy > transientAttackRatio*mean && mean > 0 {
				if s <= transientSubBlocks/2 {
					early = true
				}
				if s >= transientSubBlocks/2 {
					late = true
				}
			}
			baseline += energy
			count++
			maxEnergy = math.Max(maxEnergy, energy)
		}
	}
	e.lastEnergy = maxEnergy
	return early, late
}

// nextSequence picks the frame's window sequence — ISO/IEC 14496-3 Table
// 4.7 — keeping the chain well formed: EIGHT_SHORT is always entered via
// LONG_START (guaranteed by the lookahead) and left via LONG_STOP.
func (e *Encoder) nextSequence(transientCur, transientNext bool) int {
	inShortRun := e.prevSequence == eightShortSequence || e.prevSequence == longStartSequence
	seq := onlyLongSequence
	switch {
	case transientCur:
		seq = eightShortSequence
	case transientNext:
		seq = longStartSequence
	case inShortRun:
		seq = longStopSequence
	}
	if inShortRun && seq == longStartSequence {
		seq = eightShortSequence // squeezed transients keep the short run open
	}
	e.prevSequence = seq
	return seq
}

// groupCoding is one window group's quantization outcome; long frames have
// exactly one group, EIGHT_SHORT frames eight (one per window,
// scale_factor_grouping = 0).
type groupCoding struct {
	bands []bandCoding
}

// bandCoding is the per-scalefactor-band outcome of quantization.
type bandCoding struct {
	book        int
	scalefactor int
	quant       []int32
}

const maxFrameBitsPerChannel = 6144

// emitFrame windows and transforms frame (blockA | blockB), then writes
// raw_data_block(), enforcing the AAC-LC decoder input-buffer limit of 6144
// bits per channel per frame — ISO/IEC 14496-3 §4.5.3 — by progressively
// raising the noise floor until the complete ADTS frame fits.
func (e *Encoder) emitFrame(sequence int) ([]byte, error) {
	channels := e.config.Channels
	spectra := make([][][]float64, channels) // [channel][group][coeffs]
	for ch := range channels {
		spectra[ch] = e.analyze(sequence, e.blockA[ch], e.blockB[ch])
	}

	longOffsets, ok := tables.LongWindowSFBOffsets(e.samplingFrequencyIndex)
	if !ok {
		return nil, fmt.Errorf("no long-window SFB table for sampling_frequency_index %d", e.samplingFrequencyIndex)
	}
	offsets := longOffsets
	if sequence == eightShortSequence {
		offsets, _ = tables.ShortWindowSFBOffsets(e.samplingFrequencyIndex)
	}

	// TNS analysis precedes spectral quantization and runs exactly once.
	// encodeRawDataBlock may be retried with a coarser noise floor to meet
	// the 6144-bit frame limit, so applying the MA filter inside that retry
	// loop would incorrectly compound it.
	tns := make([]*tnsData, channels)
	if e.config.EnableTNS {
		tnsMaxSFB := min(len(offsets)-1, tables.TNSMaxBandsLong[e.samplingFrequencyIndex])
		shortWindow := sequence == eightShortSequence
		if shortWindow {
			tnsMaxSFB = min(len(offsets)-1, tables.TNSMaxBandsShort[e.samplingFrequencyIndex])
		}
		for ch := range channels {
			if shortWindow {
				flat := make([]float64, 0, SamplesPerFrame)
				for _, window := range spectra[ch] {
					flat = append(flat, window...)
				}
				tns[ch] = analyzeTNS(flat, offsets, tnsMaxSFB, true)
				for w, window := range spectra[ch] {
					copy(window, flat[w*len(window):(w+1)*len(window)])
				}
			} else {
				tns[ch] = analyzeTNS(spectra[ch][0], offsets, tnsMaxSFB, false)
			}
		}
	}

	maxPayloadBits := maxFrameBitsPerChannel*channels - adts.HeaderSize*8
	baseSF := e.baseScalefactor
	for {
		payload, err := e.encodeRawDataBlock(spectra, tns, sequence, offsets, baseSF)
		if err != nil {
			return nil, err
		}
		if len(payload)*8 <= maxPayloadBits {
			header, err := adts.Build(adts.Header{
				Profile:                1, // AAC LC (audioObjectType 2 minus one)
				SamplingFrequencyIndex: uint8(e.samplingFrequencyIndex),
				ChannelConfiguration:   uint8(channels),
				FrameLength:            adts.HeaderSize + len(payload),
				BufferFullness:         adts.VariableBitRateBufferFullness,
			})
			if err != nil {
				return nil, fmt.Errorf("build ADTS header: %w", err)
			}
			return append(header, payload...), nil
		}
		if baseSF >= 255 {
			return nil, fmt.Errorf("AAC frame uses %d payload bits, exceeds %d-bit limit for %d channels at coarsest quantization", len(payload)*8, maxPayloadBits, channels)
		}
		baseSF = min(255, baseSF+frameLimitScalefactorStep)
	}
}

// analyze runs the analysis filterbank for one channel: the sequence's
// window applied to (blockA | blockB), then the MDCT — the encoder-side
// mirror of the decoder's synthesize (ISO/IEC 14496-3 §4.6.11, Table 4.7).
// Long frames return one 1024-coefficient group; EIGHT_SHORT returns eight
// 128-coefficient groups.
func (e *Encoder) analyze(sequence int, a, b []float64) [][]float64 {
	const n = SamplesPerFrame
	frame := make([]float64, 2*n)
	copy(frame, a)
	copy(frame[n:], b)

	if sequence == eightShortSequence {
		groups := make([][]float64, 8)
		for w := range 8 {
			z := make([]float64, 2*n/8)
			base := n/2 - n/16 + (n/8)*w // 448 + 128w for n=1024
			for i := range z {
				z[i] = frame[base+i] * e.winShort[i]
			}
			groups[w] = e.mdctShort.Forward(z)
		}
		return groups
	}

	z := make([]float64, 2*n)
	switch sequence {
	case longStartSequence:
		// Long rise, flat top, short fall, zero tail.
		for i := range n {
			z[i] = frame[i] * e.winLong[i]
		}
		for i := n; i < n+448; i++ {
			z[i] = frame[i]
		}
		for i := range 128 {
			z[n+448+i] = frame[n+448+i] * e.winShort[128+i]
		}
	case longStopSequence:
		// Zero head, short rise, flat, long fall.
		for i := range 128 {
			z[448+i] = frame[448+i] * e.winShort[i]
		}
		for i := 576; i < n; i++ {
			z[i] = frame[i]
		}
		for i := n; i < 2*n; i++ {
			z[i] = frame[i] * e.winLong[i]
		}
	default: // ONLY_LONG_SEQUENCE
		for i := range 2 * n {
			z[i] = frame[i] * e.winLong[i]
		}
	}
	return [][]float64{e.mdctLong.Forward(z)}
}

// encodeRawDataBlock writes raw_data_block() with either a
// single_channel_element() or channel_pair_element() — ISO/IEC 14496-3
// Tables 4.3–4.5 and 4.44.
func (e *Encoder) encodeRawDataBlock(spectra [][][]float64, tns []*tnsData, sequence int, offsets []uint16, baseSF int) ([]byte, error) {
	type channelCoding struct {
		groups     []groupCoding
		globalGain int
		tns        *tnsData
	}
	channels := make([]channelCoding, len(spectra))
	for ch, groups := range spectra {
		if ch < len(tns) {
			channels[ch].tns = tns[ch]
		}
		channels[ch].groups = make([]groupCoding, len(groups))
		prevSF := -1
		for g, spectrum := range groups {
			channels[ch].groups[g].bands, prevSF = quantizeBands(spectrum, offsets, baseSF, prevSF)
			if channels[ch].globalGain == 0 && prevSF >= 0 {
				channels[ch].globalGain = firstScalefactor(channels[ch].groups[:g+1])
			}
		}
		channels[ch].globalGain = firstScalefactor(channels[ch].groups)
	}
	maxSFB := len(offsets) - 1

	w := bits.NewWriter()
	write := func(v uint64, n int) error { return w.WriteBits(v, n) }

	switch len(channels) {
	case 1:
		if err := write(idSCE, 3); err != nil {
			return nil, err
		}
		if err := write(0, 4); err != nil { // element_instance_tag
			return nil, err
		}
		if err := e.writeIndividualChannelStream(w, channels[0].groups, channels[0].globalGain, channels[0].tns, sequence, maxSFB, true); err != nil {
			return nil, err
		}
	case 2:
		// channel_pair_element() — ISO/IEC 14496-3 Table 4.5. With a common
		// window the ics_info is written once, followed by independent ICS
		// payloads for left and right that omit their own ics_info.
		if err := write(idCPE, 3); err != nil {
			return nil, err
		}
		if err := write(0, 4); err != nil { // element_instance_tag
			return nil, err
		}
		if err := write(1, 1); err != nil { // common_window
			return nil, err
		}
		if err := writeICSInfo(w, sequence, maxSFB); err != nil {
			return nil, err
		}
		if err := write(0, 2); err != nil { // ms_mask_present: no M/S stereo
			return nil, err
		}
		for _, channel := range channels {
			if err := e.writeIndividualChannelStream(w, channel.groups, channel.globalGain, channel.tns, sequence, maxSFB, false); err != nil {
				return nil, err
			}
		}
	default:
		return nil, fmt.Errorf("encode raw_data_block with %d channels", len(channels))
	}

	if err := write(idEND, 3); err != nil {
		return nil, err
	}
	w.Align() // byte_alignment() — ISO/IEC 14496-3 §4.4.1.
	return w.Bytes(), nil
}

// firstScalefactor returns the scalefactor of the first non-zero band
// across the groups (the global_gain), or 0 if the frame is silent.
func firstScalefactor(groups []groupCoding) int {
	for _, group := range groups {
		for _, band := range group.bands {
			if band.book != zeroHCB {
				return band.scalefactor
			}
		}
	}
	return 0
}

// writeICSInfo writes ics_info() for a sine-windowed frame — ISO/IEC
// 14496-3 Table 4.6. EIGHT_SHORT frames always use scale_factor_grouping
// = 0: eight groups of one window, which makes the spectral transmission
// order identical to the natural per-window layout.
func writeICSInfo(w *bits.BitWriter, sequence, maxSFB int) error {
	fields := []struct {
		value uint64
		width int
	}{
		{0, 1},                // ics_reserved_bit
		{uint64(sequence), 2}, // window_sequence
		{0, 1},                // window_shape: sine
	}
	if sequence == eightShortSequence {
		fields = append(fields,
			struct {
				value uint64
				width int
			}{uint64(maxSFB), 4},
			struct {
				value uint64
				width int
			}{0, 7}, // scale_factor_grouping: eight groups of one window
		)
	} else {
		fields = append(fields,
			struct {
				value uint64
				width int
			}{uint64(maxSFB), 6},
			struct {
				value uint64
				width int
			}{0, 1}, // predictor_data_present
		)
	}
	for _, field := range fields {
		if err := w.WriteBits(field.value, field.width); err != nil {
			return err
		}
	}
	return nil
}

// writeIndividualChannelStream writes individual_channel_stream() from
// global_gain through spectral_data — ISO/IEC 14496-3 Table 4.44. writeInfo
// is false for a CPE channel using common_window=1.
func (e *Encoder) writeIndividualChannelStream(w *bits.BitWriter, groups []groupCoding, globalGain int, tns *tnsData, sequence, maxSFB int, writeInfo bool) error {
	if err := w.WriteBits(uint64(globalGain), 8); err != nil {
		return err
	}
	if writeInfo {
		if err := writeICSInfo(w, sequence, maxSFB); err != nil {
			return err
		}
	}

	// section_data() — Table 4.46: within each group, adjacent bands
	// sharing a codebook merge into one section; runs at the escape value
	// extend the length field (5 bits/esc 31 long, 3 bits/esc 7 short).
	sectBits, sectEsc := 5, 31
	if sequence == eightShortSequence {
		sectBits, sectEsc = 3, 7
	}
	for _, group := range groups {
		bands := group.bands
		for start := 0; start < len(bands); {
			end := start + 1
			for end < len(bands) && bands[end].book == bands[start].book {
				end++
			}
			if err := w.WriteBits(uint64(bands[start].book), 4); err != nil {
				return err
			}
			for run := end - start; ; run -= sectEsc {
				if run < sectEsc {
					if err := w.WriteBits(uint64(run), sectBits); err != nil {
						return err
					}
					break
				}
				if err := w.WriteBits(uint64(sectEsc), sectBits); err != nil {
					return err
				}
				if run == sectEsc { // escape must be followed by a terminator
					if err := w.WriteBits(0, sectBits); err != nil {
						return err
					}
					break
				}
			}
			start = end
		}
	}

	// scale_factor_data() — Table 4.47: one DPCM chain from global_gain
	// across all groups' non-zero bands.
	sf := globalGain
	for _, group := range groups {
		for _, band := range group.bands {
			if band.book == zeroHCB {
				continue
			}
			if err := coder.EncodeScalefactorDelta(w, band.scalefactor-sf); err != nil {
				return err
			}
			sf = band.scalefactor
		}
	}

	if err := w.WriteBits(0, 1); err != nil { // pulse_data_present
		return err
	}
	present := tnsPresent(tns)
	if err := w.WriteBits(uint64(boolBit(present)), 1); err != nil { // tns_data_present
		return err
	}
	if present {
		if err := writeTNSData(w, tns, sequence == eightShortSequence); err != nil {
			return err
		}
	}
	if err := w.WriteBits(0, 1); err != nil { // gain_control_data_present
		return err
	}

	// spectral_data() — Table 4.50; with one window per group the
	// transmission order is the natural band order per group.
	for _, group := range groups {
		for _, band := range group.bands {
			if band.book == zeroHCB {
				continue
			}
			if err := coder.EncodeSpectral(w, band.book, band.quant); err != nil {
				return err
			}
		}
	}
	return nil
}

// Quality-mapping constants, calibrated against the
// external oracle: quality 0 places the uniform noise floor at the quietest
// (coarsest) base scalefactor; quality 1 lowers it by the full range.
const (
	quietestScalefactor       = 150
	qualityScalefactorRange   = 36
	frameLimitScalefactorStep = 8
)

// quantizeBands quantizes each scalefactor band against a uniform noise
// floor: every band shares the base scalefactor, raised
// per band only when needed to keep magnitudes inside the codebook-11
// escape limit. Bands that quantize to all zeros become ZERO_HCB and drop
// out of the scalefactor DPCM chain entirely, which keeps the chain's ±60
// delta limit from ever binding (loud-band overrides stay within ~54 of
// any base value; a defensive clamp guards the invariant regardless).
// prevSF carries the chain across groups; pass -1 to start a frame.
func quantizeBands(spectrum []float64, offsets []uint16, baseSF, prevSF int) (bands []bandCoding, lastSF int) {
	maxSFB := len(offsets) - 1
	bands = make([]bandCoding, maxSFB)
	for band := range bands {
		lo, hi := int(offsets[band]), int(offsets[band+1])
		amax := 0.0
		for _, x := range spectrum[lo:hi] {
			amax = math.Max(amax, math.Abs(x))
		}
		if amax == 0 {
			bands[band] = bandCoding{book: zeroHCB}
			continue
		}

		// The uniform floor, raised only if the band would overflow the
		// spectral escape limit of 8191.
		sf := max(baseSF, scalefactorFor(amax, 8191))
		if prevSF >= 0 {
			sf = min(max(sf, prevSF-60), prevSF+60) // defensive; see doc comment
		}
		sf = min(max(sf, 0), 255)

		quant := make([]int32, hi-lo)
		allZero := true
		for i, x := range spectrum[lo:hi] {
			quant[i] = quantize(x, sf)
			allZero = allZero && quant[i] == 0
		}
		if allZero {
			// Below the noise floor; ZERO_HCB says so directly and keeps
			// the band out of the DPCM chain.
			bands[band] = bandCoding{book: zeroHCB}
			continue
		}
		bands[band] = bandCoding{book: chooseCodebook(quant), scalefactor: sf, quant: quant}
		prevSF = sf
	}
	return bands, prevSF
}

// quantize implements the inverse of the ISO/IEC 14496-3 §4.6.2 dequantizer:
//
//	q = sign(x) · floor(|x|^(3/4) · 2^(−3(sf − SF_OFFSET)/16) + 0.4054)
//
// clipped to the book-11 escape limit 8191.
func quantize(x float64, sf int) int32 {
	if x == 0 {
		return 0
	}
	q := int32(math.Pow(math.Abs(x), 0.75)*math.Exp2(-3*float64(sf-coder.ScalefactorOffset)/16) + 0.4054)
	q = min(q, 8191)
	if x < 0 {
		return -q
	}
	return q
}

// scalefactorFor returns the smallest scalefactor whose quantized magnitude
// for amax does not exceed cap (smaller scalefactor = finer quantization).
// It must probe with the unclipped magnitude: quantize() clips at the
// escape limit 8191, which would make a cap of 8191 vacuously satisfied at
// every scalefactor while the actual coefficients clip audibly.
func scalefactorFor(amax float64, cap int32) int {
	magnitude := func(sf int) int64 {
		return int64(math.Pow(amax, 0.75)*math.Exp2(-3*float64(sf-coder.ScalefactorOffset)/16) + 0.4054)
	}
	// Invert the quantizer around the cap, then correct for rounding.
	sf := coder.ScalefactorOffset + int(math.Ceil(16.0/3.0*(math.Log2(math.Pow(amax, 0.75))-math.Log2(float64(cap)+1-0.4054))))
	for magnitude(sf) > int64(cap) {
		sf++
	}
	for sf > 0 && magnitude(sf-1) <= int64(cap) {
		sf--
	}
	return sf
}

// chooseCodebook picks the cheapest spectrum codebook that can represent the
// band — within each LAV class the two books are compared by exact bit cost.
func chooseCodebook(quant []int32) int {
	maxAbs := int32(0)
	for _, q := range quant {
		maxAbs = max(maxAbs, abs32(q))
	}
	var pair [2]int
	switch {
	case maxAbs <= 1:
		pair = [2]int{1, 2}
	case maxAbs <= 2:
		pair = [2]int{3, 4}
	case maxAbs <= 4:
		pair = [2]int{5, 6}
	case maxAbs <= 7:
		pair = [2]int{7, 8}
	case maxAbs <= 12:
		pair = [2]int{9, 10}
	default:
		return 11
	}
	bitsA, errA := coder.SpectralBits(pair[0], quant)
	bitsB, errB := coder.SpectralBits(pair[1], quant)
	if errA != nil || errB != nil || bitsA <= bitsB {
		return pair[0]
	}
	return pair[1]
}

func abs32(v int32) int32 {
	if v < 0 {
		return -v
	}
	return v
}

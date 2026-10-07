package aac

import (
	"errors"
	"fmt"
	"math/rand"

	"github.com/arabian9ts/aac-go/adts"
	"github.com/arabian9ts/aac-go/internal/bits"
	"github.com/arabian9ts/aac-go/internal/coder"
	"github.com/arabian9ts/aac-go/internal/tables"
	"github.com/arabian9ts/aac-go/internal/transform"
)

// Syntactic element identifiers — ISO/IEC 14496-3 Table 4.85.
const (
	idCPE = 1
	idCCE = 2
	idLFE = 3
	idDSE = 4
	idPCE = 5
	idFIL = 6
)

// Window sequences — ISO/IEC 14496-3 Table 4.7.
const (
	longStartSequence  = 1
	eightShortSequence = 2
	longStopSequence   = 3
)

// Spectrum codebook markers beyond the Huffman books — ISO/IEC 14496-3
// Table 4.132. Bands using these transmit no spectral coefficients.
const (
	reservedHCB   = 12
	noiseHCB      = 13 // PNS
	intensityHCB2 = 14 // intensity stereo, out of phase
	intensityHCB  = 15 // intensity stereo, in phase
)

// ErrUnsupported reports a stream feature that is syntactically valid AAC
// but outside the currently implemented subset (see the README). The error
// message names the missing tool; extending support means implementing the
// tool, not changing callers.
var ErrUnsupported = errors.New("AAC feature is not supported")

func unsupportedf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrUnsupported}, args...)...)
}

// Decoder incrementally decodes ADTS-framed AAC-LC data.
//
// The stream format (sample rate, channel layout) is locked in by the first
// frame. Construct with NewDecoder.
type Decoder struct {
	pending []byte // undecoded input tail, always starting at a frame boundary

	configured             bool
	sampleRate             int
	channels               int
	samplingFrequencyIndex int

	mdctLong  *transform.MDCT
	mdctShort *transform.MDCT
	longWin   [2][]float64 // indexed by window shape: 0 sine, 1 KBD
	shortWin  [2][]float64
	// Per-channel overlap state. saved holds the previous frame's tail with
	// deferred windowing (see synthesize): raw for long frames, self-windowed
	// except the final short block for EIGHT_SHORT frames.
	saved     [][]float64
	prevSeq   []int      // per channel: previous frame's window_sequence
	prevShape []int      // per channel: previous frame's window shape
	noise     *rand.Rand // PNS noise source (only the distribution is normative)
}

// icsInfo is the parsed ics_info() — the window and band layout shared by
// the syntax and the filterbank — ISO/IEC 14496-3 Table 4.6.
type icsInfo struct {
	windowSequence int
	windowShape    int
	maxSFB         int
	numWindows     int // 8 for EIGHT_SHORT, else 1
	numGroups      int
	groupLen       []int    // windows per group
	offsets        []uint16 // active scalefactor band boundaries (long or short)
}

// channelState is one channel's decoded individual_channel_stream.
type channelState struct {
	ics       *icsInfo
	bandBooks []int // [g*maxSFB+sfb]
	// bandVals meaning depends on the band's book: absolute scalefactor for
	// spectrum books, the PNS energy accumulator for noiseHCB, and
	// is_position for the intensity books.
	bandVals []int
	spectrum []float64 // 1024 coefficients, each window contiguous 128
	tns      *tnsData
}

// NewDecoder returns an AAC streaming decoder. The format is detected from
// the first ADTS header seen by Decode.
func NewDecoder() (*Decoder, error) {
	return &Decoder{noise: rand.New(rand.NewSource(1))}, nil
}

// SampleRate returns the stream's sample rate in Hz, or 0 before the first
// decoded frame.
func (d *Decoder) SampleRate() int { return d.sampleRate }

// Channels returns the stream's channel count, or 0 before the first
// decoded frame.
func (d *Decoder) Channels() int { return d.channels }

// Decode consumes ADTS bytes and returns the PCM decoded so far. Input may
// be sliced arbitrarily: a partial trailing frame is buffered until the
// remaining bytes arrive (ISO/IEC 14496-3 §1.A.3 adts_frame).
func (d *Decoder) Decode(data []byte) ([]int16, error) {
	d.pending = append(d.pending, data...)

	var out []int16
	for {
		if len(d.pending) < adts.HeaderSize {
			return out, nil
		}
		header, err := adts.Parse(d.pending)
		if err != nil {
			return out, fmt.Errorf("aac: ADTS header: %w", err)
		}
		if len(d.pending) < header.FrameLength {
			return out, nil // wait for the rest of the frame
		}
		if err := d.configure(header); err != nil {
			return out, err
		}
		pcm, err := d.decodeRawDataBlock(d.pending[header.PayloadOffset():header.FrameLength])
		if err != nil {
			return out, err
		}
		out = append(out, pcm...)
		d.pending = d.pending[header.FrameLength:]
	}
}

// configure locks the stream format to the first header and rejects
// mid-stream format changes.
func (d *Decoder) configure(h adts.Header) error {
	if d.configured {
		if int(h.SamplingFrequencyIndex) != d.samplingFrequencyIndex || int(h.ChannelConfiguration) != d.channels {
			return unsupportedf("mid-stream format change")
		}
		return nil
	}
	if h.NumRawDataBlocks != 0 {
		return unsupportedf("number_of_raw_data_blocks_in_frame %d", h.NumRawDataBlocks)
	}
	rate, ok := tables.SamplingFrequency(int(h.SamplingFrequencyIndex))
	if !ok {
		return fmt.Errorf("aac: invalid sampling_frequency_index %d", h.SamplingFrequencyIndex)
	}
	if h.ChannelConfiguration != 1 && h.ChannelConfiguration != 2 {
		return unsupportedf("channel_configuration %d (mono and stereo only)", h.ChannelConfiguration)
	}
	d.sampleRate = rate
	d.samplingFrequencyIndex = int(h.SamplingFrequencyIndex)
	d.channels = int(h.ChannelConfiguration)
	d.mdctLong = transform.New(2 * SamplesPerFrame)
	d.mdctShort = transform.New(2 * SamplesPerFrame / 8)
	d.longWin[0] = transform.SineWindow(2 * SamplesPerFrame)
	d.longWin[1] = transform.KBDWindow(2*SamplesPerFrame, 4)
	d.shortWin[0] = transform.SineWindow(2 * SamplesPerFrame / 8)
	d.shortWin[1] = transform.KBDWindow(2*SamplesPerFrame/8, 6)
	d.saved = make([][]float64, d.channels)
	d.prevSeq = make([]int, d.channels)
	d.prevShape = make([]int, d.channels)
	for i := range d.saved {
		d.saved[i] = make([]float64, SamplesPerFrame)
	}
	d.configured = true
	return nil
}

// decodeRawDataBlock walks raw_data_block() — ISO/IEC 14496-3 Table 4.3 —
// decoding elements until ID_END and returning one frame of interleaved PCM.
func (d *Decoder) decodeRawDataBlock(payload []byte) ([]int16, error) {
	r := bits.NewReader(payload)
	var chans []*channelState

	for {
		id, err := r.ReadBits(3)
		if err != nil {
			return nil, fmt.Errorf("aac: id_syn_ele: %w", err)
		}
		switch id {
		case idSCE:
			ch, err := d.decodeSCE(r)
			if err != nil {
				return nil, err
			}
			chans = append(chans, ch)
		case idCPE:
			left, right, err := d.decodeCPE(r)
			if err != nil {
				return nil, err
			}
			chans = append(chans, left, right)
		case idDSE:
			if err := skipDSE(r); err != nil {
				return nil, err
			}
		case idFIL:
			if err := skipFIL(r); err != nil {
				return nil, err
			}
		case idPCE:
			if err := skipPCE(r); err != nil {
				return nil, err
			}
		case idCCE, idLFE:
			return nil, unsupportedf("syntactic element id %d", id)
		case idEND:
			if len(chans) != d.channels {
				return nil, fmt.Errorf("aac: frame carries %d channels, header says %d", len(chans), d.channels)
			}
			return d.reconstruct(chans), nil
		}
	}
}

// decodeSCE decodes single_channel_element() — ISO/IEC 14496-3 Table 4.4.
func (d *Decoder) decodeSCE(r *bits.BitReader) (*channelState, error) {
	if _, err := r.ReadBits(4); err != nil { // element_instance_tag
		return nil, err
	}
	ch, err := d.decodeICS(r, nil)
	if err != nil {
		return nil, err
	}
	d.applyChannelTNS(ch)
	return ch, nil
}

// decodeCPE decodes channel_pair_element() — ISO/IEC 14496-3 Table 4.5 —
// and applies the stereo tools in decoding order: both channels' ICS,
// then M/S, then intensity, then per-channel TNS.
func (d *Decoder) decodeCPE(r *bits.BitReader) (left, right *channelState, err error) {
	if _, err := r.ReadBits(4); err != nil { // element_instance_tag
		return nil, nil, err
	}
	commonWindow, err := r.ReadBit()
	if err != nil {
		return nil, nil, err
	}

	var shared *icsInfo
	var msMask []bool
	msPresent := uint64(0)
	if commonWindow {
		if shared, err = d.parseICSInfo(r); err != nil {
			return nil, nil, err
		}
		if msPresent, err = r.ReadBits(2); err != nil {
			return nil, nil, err
		}
		switch msPresent {
		case 0:
		case 1, 2:
			msMask = make([]bool, shared.numGroups*shared.maxSFB)
			for i := range msMask {
				if msPresent == 2 {
					msMask[i] = true
					continue
				}
				bit, err := r.ReadBit()
				if err != nil {
					return nil, nil, err
				}
				msMask[i] = bit
			}
		default:
			return nil, nil, fmt.Errorf("aac: ms_mask_present 3 is reserved")
		}
	}

	if left, err = d.decodeICS(r, shared); err != nil {
		return nil, nil, err
	}
	if right, err = d.decodeICS(r, shared); err != nil {
		return nil, nil, err
	}

	if commonWindow && msPresent != 0 {
		applyCPEMidSide(left, right, msMask)
	}
	applyCPEIntensity(left, right, msMask)
	d.applyChannelTNS(left)
	d.applyChannelTNS(right)
	return left, right, nil
}

// applyCPEMidSide undoes M/S coding on every masked band whose codebooks on
// both channels are ordinary spectrum books (noise and intensity bands are
// exempt) — ISO/IEC 14496-3 §4.6.8.1.3.
func applyCPEMidSide(left, right *channelState, msMask []bool) {
	forEachBandWindow(left.ics, func(idx, lo, hi int) {
		if !msMask[idx] || left.bandBooks[idx] >= noiseHCB || right.bandBooks[idx] >= noiseHCB {
			return
		}
		applyMidSide(left.spectrum[lo:hi], right.spectrum[lo:hi])
	})
}

// applyCPEIntensity reconstructs intensity-stereo bands of the right
// channel; the sign combines the codebook (in phase / out of phase) with
// the M/S mask — ISO/IEC 14496-3 §4.6.8.2.3.
func applyCPEIntensity(left, right *channelState, msMask []bool) {
	forEachBandWindow(right.ics, func(idx, lo, hi int) {
		book := right.bandBooks[idx]
		if book != intensityHCB && book != intensityHCB2 {
			return
		}
		invert := book == intensityHCB2
		if msMask != nil && msMask[idx] {
			invert = !invert
		}
		applyIntensity(left.spectrum[lo:hi], right.spectrum[lo:hi], right.bandVals[idx], invert)
	})
}

// applyChannelTNS runs the TNS synthesis filters over each window, with
// the filtered range clamped to tns_max_bands — ISO/IEC 14496-3 §4.6.9.
func (d *Decoder) applyChannelTNS(ch *channelState) {
	if ch.tns == nil {
		return
	}
	maxBands := tables.TNSMaxBandsLong[d.samplingFrequencyIndex]
	if ch.ics.windowSequence == eightShortSequence {
		maxBands = tables.TNSMaxBandsShort[d.samplingFrequencyIndex]
	}
	limit := min(ch.ics.maxSFB, maxBands)
	for w := range ch.ics.numWindows {
		applyTNS(ch.spectrum, ch.tns, w, ch.ics.offsets, limit)
	}
}

// forEachBandWindow invokes fn for every (group, sfb, window-in-group)
// combination with the spectrum range [lo, hi) of that window's band and
// the flattened band index g*maxSFB+sfb.
func forEachBandWindow(ics *icsInfo, fn func(idx, lo, hi int)) {
	win := 0
	for g := range ics.numGroups {
		for sfb := range ics.maxSFB {
			idx := g*ics.maxSFB + sfb
			for w := range ics.groupLen[g] {
				base := (win + w) * 128 * 8 / ics.numWindows // 128 for short, 1024 for long
				lo := base + int(ics.offsets[sfb])
				hi := base + int(ics.offsets[sfb+1])
				fn(idx, lo, hi)
			}
		}
		win += ics.groupLen[g]
	}
}

// decodeICS decodes individual_channel_stream() — ISO/IEC 14496-3 Table
// 4.44 — through spectral data, dequantization, and PNS synthesis. shared
// is the CPE common-window ics_info, or nil to parse one from the stream.
func (d *Decoder) decodeICS(r *bits.BitReader, shared *icsInfo) (*channelState, error) {
	globalGain, err := r.ReadBits(8)
	if err != nil {
		return nil, err
	}
	ics := shared
	if ics == nil {
		if ics, err = d.parseICSInfo(r); err != nil {
			return nil, err
		}
	}

	ch := &channelState{ics: ics}
	if err := d.parseSectionData(r, ch); err != nil {
		return nil, err
	}
	if err := parseScalefactorData(r, ch, int(globalGain)); err != nil {
		return nil, err
	}

	pulsePresent, err := r.ReadBit()
	if err != nil {
		return nil, err
	}
	if pulsePresent {
		return nil, unsupportedf("pulse_data")
	}
	tnsPresent, err := r.ReadBit()
	if err != nil {
		return nil, err
	}
	if tnsPresent {
		if ch.tns, err = parseTNS(r, ics.windowSequence); err != nil {
			return nil, err
		}
	}
	gainPresent, err := r.ReadBit()
	if err != nil {
		return nil, err
	}
	if gainPresent {
		return nil, unsupportedf("gain_control_data (SSR)")
	}

	if err := d.decodeSpectra(r, ch); err != nil {
		return nil, err
	}
	return ch, nil
}

// parseICSInfo reads ics_info() — ISO/IEC 14496-3 Table 4.6.
func (d *Decoder) parseICSInfo(r *bits.BitReader) (*icsInfo, error) {
	if _, err := r.ReadBit(); err != nil { // ics_reserved_bit
		return nil, err
	}
	windowSequence, err := r.ReadBits(2)
	if err != nil {
		return nil, err
	}
	windowShape, err := r.ReadBit()
	if err != nil {
		return nil, err
	}
	ics := &icsInfo{windowSequence: int(windowSequence)}
	if windowShape {
		ics.windowShape = 1
	}

	if ics.windowSequence == eightShortSequence {
		maxSFB, err := r.ReadBits(4)
		if err != nil {
			return nil, err
		}
		grouping, err := r.ReadBits(7)
		if err != nil {
			return nil, err
		}
		ics.maxSFB = int(maxSFB)
		ics.numWindows = 8
		// scale_factor_grouping: bit i (MSB first) set means window i+1
		// joins the previous window's group.
		ics.groupLen = []int{1}
		for i := 1; i < 8; i++ {
			if grouping&(1<<(7-i)) != 0 {
				ics.groupLen[len(ics.groupLen)-1]++
			} else {
				ics.groupLen = append(ics.groupLen, 1)
			}
		}
		ics.numGroups = len(ics.groupLen)
		ics.offsets, _ = tables.ShortWindowSFBOffsets(d.samplingFrequencyIndex)
	} else {
		maxSFB, err := r.ReadBits(6)
		if err != nil {
			return nil, err
		}
		predictorPresent, err := r.ReadBit()
		if err != nil {
			return nil, err
		}
		if predictorPresent {
			return nil, unsupportedf("MAIN-profile prediction")
		}
		ics.maxSFB = int(maxSFB)
		ics.numWindows = 1
		ics.numGroups = 1
		ics.groupLen = []int{1}
		ics.offsets, _ = tables.LongWindowSFBOffsets(d.samplingFrequencyIndex)
	}
	if ics.offsets == nil {
		return nil, unsupportedf("no SFB table for sampling_frequency_index %d", d.samplingFrequencyIndex)
	}
	if ics.maxSFB > len(ics.offsets)-1 {
		return nil, fmt.Errorf("aac: max_sfb %d exceeds %d bands", ics.maxSFB, len(ics.offsets)-1)
	}
	return ics, nil
}

// parseSectionData reads section_data() — ISO/IEC 14496-3 Table 4.46 — one
// section run per group, expanding to one codebook per band.
func (d *Decoder) parseSectionData(r *bits.BitReader, ch *channelState) error {
	ics := ch.ics
	bitsLen, escVal := 5, 31
	if ics.windowSequence == eightShortSequence {
		bitsLen, escVal = 3, 7
	}
	ch.bandBooks = make([]int, ics.numGroups*ics.maxSFB)
	for g := range ics.numGroups {
		for k := 0; k < ics.maxSFB; {
			book, err := r.ReadBits(4)
			if err != nil {
				return fmt.Errorf("aac: sect_cb: %w", err)
			}
			if book == reservedHCB {
				return fmt.Errorf("aac: sect_cb 12 is reserved")
			}
			sectionLen := 0
			for {
				incr, err := r.ReadBits(bitsLen)
				if err != nil {
					return fmt.Errorf("aac: sect_len_incr: %w", err)
				}
				sectionLen += int(incr)
				if int(incr) != escVal {
					break
				}
			}
			if sectionLen == 0 || k+sectionLen > ics.maxSFB {
				return fmt.Errorf("aac: section of %d bands at band %d exceeds max_sfb %d", sectionLen, k, ics.maxSFB)
			}
			for range sectionLen {
				ch.bandBooks[g*ics.maxSFB+k] = int(book)
				k++
			}
		}
	}
	return nil
}

// parseScalefactorData reads scale_factor_data() — ISO/IEC 14496-3 Table
// 4.47, §4.6.2/4.6.8.2/4.6.13 — maintaining the three DPCM chains:
// scalefactors from global_gain, PNS energies from global_gain−90 (first
// noise band PCM-coded with 9 bits), and intensity positions from 0. The
// clamp ranges and chain starts are verified against reference decoders
// in the acceptance suite.
func parseScalefactorData(r *bits.BitReader, ch *channelState, globalGain int) error {
	const noiseOffset = 90
	ics := ch.ics
	ch.bandVals = make([]int, len(ch.bandBooks))
	sf := globalGain
	noise := globalGain - noiseOffset
	position := 0
	firstNoise := true
	for idx, book := range ch.bandBooks {
		switch book {
		case zeroHCB:
		case intensityHCB, intensityHCB2:
			delta, err := coder.DecodeScalefactorDelta(r)
			if err != nil {
				return err
			}
			position += delta
			ch.bandVals[idx] = clamp(position, -155, 100)
		case noiseHCB:
			if firstNoise {
				pcm, err := r.ReadBits(9)
				if err != nil {
					return err
				}
				noise += int(pcm) - 256
				firstNoise = false
			} else {
				delta, err := coder.DecodeScalefactorDelta(r)
				if err != nil {
					return err
				}
				noise += delta
			}
			ch.bandVals[idx] = clamp(noise, -100, 155)
		default:
			delta, err := coder.DecodeScalefactorDelta(r)
			if err != nil {
				return err
			}
			sf += delta
			if sf < 0 || sf > 255 {
				return fmt.Errorf("aac: scalefactor %d out of range at band %d", sf, idx%ics.maxSFB)
			}
			ch.bandVals[idx] = sf
		}
	}
	return nil
}

// decodeSpectra reads spectral_data() — ISO/IEC 14496-3 Table 4.50 — then
// deinterleaves the grouped transmission order into per-window spectra,
// dequantizes, and synthesizes PNS bands.
func (d *Decoder) decodeSpectra(r *bits.BitReader, ch *channelState) error {
	ics := ch.ics
	quant := make([]int32, SamplesPerFrame) // grouped transmission order
	cursor := 0
	type bandSpan struct{ idx, start, width int } // start within the grouped buffer
	spans := make([]bandSpan, 0, len(ch.bandBooks))

	for g := range ics.numGroups {
		for sfb := range ics.maxSFB {
			idx := g*ics.maxSFB + sfb
			width := int(ics.offsets[sfb+1] - ics.offsets[sfb])
			n := width * ics.groupLen[g]
			spans = append(spans, bandSpan{idx, cursor, width})
			book := ch.bandBooks[idx]
			if book >= 1 && book <= 11 {
				if cursor+n > len(quant) {
					return fmt.Errorf("aac: spectral data overruns frame at band %d", idx)
				}
				if err := coder.DecodeSpectral(r, book, quant[cursor:cursor+n]); err != nil {
					return fmt.Errorf("aac: band %d: %w", idx, err)
				}
			}
			cursor += n
		}
		// Skip the coefficients above max_sfb for this group.
		cursor = groupEnd(ics, g)
	}

	// Deinterleave to per-window 128-coefficient spectra (identity for long
	// windows), dequantize, and fill noise bands.
	ch.spectrum = make([]float64, SamplesPerFrame)
	win := 0
	spanIdx := 0
	for g := range ics.numGroups {
		for sfb := range ics.maxSFB {
			span := spans[spanIdx]
			spanIdx++
			idx := span.idx
			book := ch.bandBooks[idx]
			for w := range ics.groupLen[g] {
				windowBase := (win + w) * (SamplesPerFrame / ics.numWindows)
				lo := windowBase + int(ics.offsets[sfb])
				switch {
				case book == zeroHCB, book == intensityHCB, book == intensityHCB2:
					// Zero, or reconstructed later from the left channel.
				case book == noiseHCB:
					fillPNS(ch.spectrum[lo:lo+span.width], ch.bandVals[idx], d.noise)
				default:
					gain := coder.BandGain(ch.bandVals[idx])
					src := span.start + w*span.width
					for k := range span.width {
						ch.spectrum[lo+k] = coder.DequantizeWithGain(quant[src+k], gain)
					}
				}
			}
		}
		win += ics.groupLen[g]
	}
	return nil
}

// groupEnd returns the grouped-buffer offset just past group g.
func groupEnd(ics *icsInfo, g int) int {
	windows := 0
	for i := 0; i <= g; i++ {
		windows += ics.groupLen[i]
	}
	return windows * (SamplesPerFrame / ics.numWindows)
}

// reconstruct runs the synthesis filterbank per channel — ISO/IEC 14496-3
// §4.6.11 — and interleaves the channels as clamped int16 PCM.
func (d *Decoder) reconstruct(chans []*channelState) []int16 {
	out := make([]int16, SamplesPerFrame*d.channels)
	for chIdx, ch := range chans {
		pcm := d.synthesize(chIdx, ch)
		for i, v := range pcm {
			out[i*d.channels+chIdx] = clampInt16(v)
		}
	}
	return out
}

// synthesize converts one channel's spectrum to 1024 time samples. The
// falling window on the previous frame's tail is chosen by the *current*
// frame — the convention interoperable decoders use so that "meaningless"
// transitions (real encoders emit ONLY_LONG directly into EIGHT_SHORT with
// no LONG_START) reconstruct correctly: both sides then treat the boundary
// as a short-style overlap. Verified against reference decoders on such
// streams in the acceptance suite.
//
// The per-channel saved tail therefore carries deferred windowing: raw for
// long frames; for EIGHT_SHORT frames the block overlaps are already
// resolved except the final block's falling half, which stays raw.
func (d *Decoder) synthesize(chIdx int, ch *channelState) []float64 {
	const n = SamplesPerFrame
	ics := ch.ics
	prevSeq, prevShape := d.prevSeq[chIdx], d.prevShape[chIdx]
	curSeq, curShape := ics.windowSequence, ics.windowShape
	saved := d.saved[chIdx]

	// Long-to-long overlaps use the full long window; every other pairing —
	// anything touching EIGHT_SHORT, LONG_START's tail, or LONG_STOP's head —
	// is a short-style overlap in the 128-sample window at [448, 576).
	longOverlap := (prevSeq == 0 || prevSeq == longStopSequence) &&
		(curSeq == 0 || curSeq == longStartSequence)

	// Current frame's contribution with the tail's windowing deferred.
	cur := make([]float64, 2*n)
	if curSeq == eightShortSequence {
		for b := range 8 {
			x := d.mdctShort.Inverse(ch.spectrum[b*128 : (b+1)*128])
			riseShape := curShape
			if b == 0 {
				riseShape = prevShape
			}
			base := 448 + 128*b
			for i := range 128 {
				cur[base+i] += x[i] * d.shortWin[riseShape][i]
			}
			if b < 7 {
				for i := range 128 {
					cur[base+128+i] += x[128+i] * d.shortWin[curShape][128+i]
				}
			} else {
				// The last block's falling half stays raw: the next frame
				// windows it as part of its own overlap.
				for i := range 128 {
					cur[base+128+i] += x[128+i]
				}
			}
		}
	} else {
		x := d.mdctLong.Inverse(ch.spectrum)
		if longOverlap {
			for i := range n {
				cur[i] = x[i] * d.longWin[prevShape][i]
			}
		} else {
			// Short-style left edge regardless of the signaled sequence:
			// zeros, a short rise in the previous shape, then flat.
			for i := range 128 {
				cur[448+i] = x[448+i] * d.shortWin[prevShape][i]
			}
			for i := 576; i < n; i++ {
				cur[i] = x[i]
			}
		}
		copy(cur[n:], x[n:]) // raw tail
	}

	out := make([]float64, n)
	if longOverlap {
		w := d.longWin[prevShape]
		for i := range n {
			out[i] = saved[i]*w[n+i] + cur[i]
		}
	} else {
		sw := d.shortWin[prevShape]
		for i := range 448 {
			out[i] = saved[i] + cur[i]
		}
		for i := range 128 {
			out[448+i] = saved[448+i]*sw[128+i] + cur[448+i]
		}
		// Beyond the overlap window the previous tail is in its zero
		// region (retroactive LONG_START shape); only the current frame
		// contributes.
		copy(out[576:], cur[576:n])
	}

	copy(saved, cur[n:])
	d.prevSeq[chIdx] = curSeq
	d.prevShape[chIdx] = curShape
	return out
}

func clampInt16(v float64) int16 {
	switch {
	case v >= 32767:
		return 32767
	case v <= -32768:
		return -32768
	case v >= 0:
		return int16(v + 0.5)
	default:
		return int16(v - 0.5)
	}
}

func clamp(v, lo, hi int) int {
	return min(max(v, lo), hi)
}

// skipDSE skips data_stream_element() — ISO/IEC 14496-3 Table 4.10.
func skipDSE(r *bits.BitReader) error {
	if _, err := r.ReadBits(4); err != nil { // element_instance_tag
		return err
	}
	alignFlag, err := r.ReadBit()
	if err != nil {
		return err
	}
	count, err := r.ReadBits(8)
	if err != nil {
		return err
	}
	if count == 255 {
		extra, err := r.ReadBits(8)
		if err != nil {
			return err
		}
		count += extra
	}
	if alignFlag {
		alignReader(r)
	}
	for range count {
		if _, err := r.ReadBits(8); err != nil {
			return err
		}
	}
	return nil
}

// skipFIL skips fill_element() — ISO/IEC 14496-3 Table 4.11 — but detects
// SBR extension payloads: silently skipping them would return the low-band
// core signal of an HE-AAC stream as if it were the full decode.
func skipFIL(r *bits.BitReader) error {
	count, err := r.ReadBits(4)
	if err != nil {
		return err
	}
	if count == 15 {
		extra, err := r.ReadBits(8)
		if err != nil {
			return err
		}
		count += extra - 1
	}
	if count == 0 {
		return nil
	}
	// extension_payload() — Table 4.51.
	extensionType, err := r.ReadBits(4)
	if err != nil {
		return err
	}
	const extSBRData, extSBRDataCRC = 0xd, 0xe
	if extensionType == extSBRData || extensionType == extSBRDataCRC {
		return unsupportedf("HE-AAC/SBR extension (decode would drop the high band)")
	}
	for i := uint64(0); i < count*8-4; i++ {
		if _, err := r.ReadBits(1); err != nil {
			return err
		}
	}
	return nil
}

// skipPCE parses and discards program_config_element() — ISO/IEC 14496-3
// Table 4.2 — so streams with channel_configuration 0 keep bit alignment.
func skipPCE(r *bits.BitReader) error {
	skip := func(n int) error {
		_, err := r.ReadBits(n)
		return err
	}
	if err := skip(4 + 2 + 4); err != nil { // instance tag, object_type, sampling_frequency_index
		return err
	}
	counts, err := r.ReadBits(4 + 4 + 4 + 2 + 3 + 4)
	if err != nil {
		return err
	}
	numFront := int(counts >> 17 & 0xf)
	numSide := int(counts >> 13 & 0xf)
	numBack := int(counts >> 9 & 0xf)
	numLFE := int(counts >> 7 & 0x3)
	numAssoc := int(counts >> 4 & 0x7)
	numCC := int(counts & 0xf)

	for _, present := range []int{4, 4, 3} { // mono/stereo mixdown, matrix mixdown
		flag, err := r.ReadBit()
		if err != nil {
			return err
		}
		if flag {
			if err := skip(present); err != nil {
				return err
			}
		}
	}
	for range numFront + numSide + numBack {
		if err := skip(1 + 4); err != nil { // element_is_cpe, tag_select
			return err
		}
	}
	for range numLFE + numAssoc {
		if err := skip(4); err != nil {
			return err
		}
	}
	for range numCC {
		if err := skip(1 + 4); err != nil { // cc_element_is_ind_sw, tag
			return err
		}
	}
	alignReader(r)
	commentBytes, err := r.ReadBits(8)
	if err != nil {
		return err
	}
	for range commentBytes {
		if err := skip(8); err != nil {
			return err
		}
	}
	return nil
}

// alignReader consumes bits through the next byte boundary.
func alignReader(r *bits.BitReader) {
	if rem := r.Position() % 8; rem != 0 {
		_, _ = r.ReadBits(8 - rem)
	}
}

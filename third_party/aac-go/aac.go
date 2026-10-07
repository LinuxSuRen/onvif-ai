// Package aac implements a streaming AAC-LC (ISO/IEC 14496-3) encoder and
// decoder in pure Go, producing and consuming ADTS streams.
//
// See the README for the implemented subset and the extension seams: long
// windows only, mono SCE and stereo CPE, 44.1/48 kHz.
package aac

import (
	"errors"
	"fmt"

	"github.com/arabian9ts/aac-go/internal/tables"
)

const (
	// SamplesPerFrame is the AAC-LC frame length for a long window.
	SamplesPerFrame = 1024

	idSCE = 0
	idEND = 7

	onlyLongSequence = 0
	zeroHCB          = 0
)

var ErrEncoderFlushed = errors.New("AAC encoder has already been flushed")

// Config configures an AAC encoder.
type Config struct {
	SampleRate int
	Channels   int
	// Quality in [0, 1] trades bitrate for fidelity (VBR). It maps
	// linearly onto the encoder's uniform quantization noise floor.
	Quality float64
	// EnableTNS enables encoder-side Temporal Noise Shaping. It is opt-in and
	// best suited to transient-dominant material; it is not recommended for
	// tone-dominant material because this simple predictor can reduce its SNR
	// Decoding TNS is always enabled.
	EnableTNS bool
}

func (c Config) validate() (samplingFrequencyIndex int, err error) {
	if c.Channels != 1 && c.Channels != 2 {
		return 0, fmt.Errorf("AAC encoder supports one or two channels, got %d", c.Channels)
	}
	index, ok := tables.SamplingFrequencyIndex(c.SampleRate)
	if !ok || (c.SampleRate != 44100 && c.SampleRate != 48000) {
		return 0, fmt.Errorf("AAC encoder supports 44100 or 48000 Hz, got %d", c.SampleRate)
	}
	if !(c.Quality >= 0 && c.Quality <= 1) {
		return 0, fmt.Errorf("AAC quality %g is outside [0, 1]", c.Quality)
	}
	return index, nil
}

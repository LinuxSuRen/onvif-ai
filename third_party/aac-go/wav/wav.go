// Package wav provides a minimal RIFF/WAVE reader and writer for 16-bit
// little-endian PCM. It exists to support tests and CLI tools; it is not a
// general-purpose WAV library.
package wav

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Write serializes interleaved 16-bit PCM samples as a canonical 44-byte
// header RIFF/WAVE file.
func Write(w io.Writer, sampleRate, channels int, pcm []int16) error {
	if channels <= 0 {
		return fmt.Errorf("wav: invalid channel count %d", channels)
	}
	if sampleRate <= 0 {
		return fmt.Errorf("wav: invalid sample rate %d", sampleRate)
	}
	dataSize := len(pcm) * 2
	blockAlign := channels * 2
	header := make([]byte, 0, 44)
	le := binary.LittleEndian

	header = append(header, "RIFF"...)
	header = le.AppendUint32(header, uint32(36+dataSize))
	header = append(header, "WAVE"...)
	header = append(header, "fmt "...)
	header = le.AppendUint32(header, 16)                            // fmt chunk size
	header = le.AppendUint16(header, 1)                             // PCM format
	header = le.AppendUint16(header, uint16(channels))              //nolint:gosec
	header = le.AppendUint32(header, uint32(sampleRate))            //nolint:gosec
	header = le.AppendUint32(header, uint32(sampleRate*blockAlign)) // byte rate
	header = le.AppendUint16(header, uint16(blockAlign))            //nolint:gosec
	header = le.AppendUint16(header, 16)                            // bits per sample
	header = append(header, "data"...)
	header = le.AppendUint32(header, uint32(dataSize))
	if _, err := w.Write(header); err != nil {
		return fmt.Errorf("wav: writing header: %w", err)
	}

	buf := make([]byte, len(pcm)*2)
	for i, s := range pcm {
		le.PutUint16(buf[i*2:], uint16(s)) //nolint:gosec
	}
	if _, err := w.Write(buf); err != nil {
		return fmt.Errorf("wav: writing samples: %w", err)
	}
	return nil
}

// Read parses a RIFF/WAVE stream containing 16-bit PCM and returns the
// sample rate, channel count, and interleaved samples. Chunks other than
// "fmt " and "data" are skipped.
func Read(r io.Reader) (sampleRate, channels int, pcm []int16, err error) {
	var riff [12]byte
	if _, err = io.ReadFull(r, riff[:]); err != nil {
		return 0, 0, nil, fmt.Errorf("wav: reading RIFF header: %w", err)
	}
	if string(riff[0:4]) != "RIFF" || string(riff[8:12]) != "WAVE" {
		return 0, 0, nil, fmt.Errorf("wav: not a RIFF/WAVE stream")
	}

	le := binary.LittleEndian
	haveFmt := false
	for {
		var chunkHeader [8]byte
		if _, err = io.ReadFull(r, chunkHeader[:]); err != nil {
			if err == io.EOF {
				return 0, 0, nil, fmt.Errorf("wav: missing data chunk")
			}
			return 0, 0, nil, fmt.Errorf("wav: reading chunk header: %w", err)
		}
		chunkID := string(chunkHeader[0:4])
		chunkSize := int(le.Uint32(chunkHeader[4:8]))

		switch chunkID {
		case "fmt ":
			fmtChunk := make([]byte, chunkSize)
			if _, err = io.ReadFull(r, fmtChunk); err != nil {
				return 0, 0, nil, fmt.Errorf("wav: reading fmt chunk: %w", err)
			}
			if len(fmtChunk) < 16 {
				return 0, 0, nil, fmt.Errorf("wav: fmt chunk too short (%d bytes)", len(fmtChunk))
			}
			switch format := le.Uint16(fmtChunk[0:2]); format {
			case 1: // WAVE_FORMAT_PCM
			case 0xfffe: // WAVE_FORMAT_EXTENSIBLE: PCM iff the SubFormat GUID starts 0x0001
				if len(fmtChunk) < 26 || le.Uint16(fmtChunk[24:26]) != 1 {
					return 0, 0, nil, fmt.Errorf("wav: extensible format is not PCM")
				}
			default:
				return 0, 0, nil, fmt.Errorf("wav: unsupported format tag %d (want PCM)", format)
			}
			if bits := le.Uint16(fmtChunk[14:16]); bits != 16 {
				return 0, 0, nil, fmt.Errorf("wav: unsupported bit depth %d (want 16)", bits)
			}
			channels = int(le.Uint16(fmtChunk[2:4]))
			sampleRate = int(le.Uint32(fmtChunk[4:8]))
			haveFmt = true
		case "data":
			if !haveFmt {
				return 0, 0, nil, fmt.Errorf("wav: data chunk before fmt chunk")
			}
			raw := make([]byte, chunkSize)
			if _, err = io.ReadFull(r, raw); err != nil {
				return 0, 0, nil, fmt.Errorf("wav: reading data chunk: %w", err)
			}
			pcm = make([]int16, chunkSize/2)
			for i := range pcm {
				pcm[i] = int16(le.Uint16(raw[i*2:])) //nolint:gosec
			}
			return sampleRate, channels, pcm, nil
		default:
			// Skip unknown chunks (LIST, fact, ...), honoring RIFF word alignment.
			skip := chunkSize + chunkSize%2
			if _, err = io.CopyN(io.Discard, r, int64(skip)); err != nil {
				return 0, 0, nil, fmt.Errorf("wav: skipping %q chunk: %w", chunkID, err)
			}
		}
	}
}

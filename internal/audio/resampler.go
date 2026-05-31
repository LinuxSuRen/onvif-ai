package audio

import (
	"encoding/binary"
	"fmt"
)

// ResamplePCM resamples 16-bit PCM audio from input sample rate to output sample rate.
// Uses linear interpolation. For production, consider using a proper resampling library.
// Input: PCM 16-bit signed little-endian samples.
// Output: PCM 16-bit signed little-endian samples at the new rate.
func ResamplePCM(pcm []byte, inputRate, outputRate int) ([]byte, error) {
	if len(pcm)%2 != 0 {
		return nil, fmt.Errorf("PCM data must be 16-bit (even byte length), got %d", len(pcm))
	}
	if inputRate <= 0 || outputRate <= 0 {
		return nil, fmt.Errorf("sample rates must be positive")
	}
	if inputRate == outputRate {
		dst := make([]byte, len(pcm))
		copy(dst, pcm)
		return dst, nil
	}

	sampleCount := len(pcm) / 2
	ratio := float64(outputRate) / float64(inputRate)
	outputSamples := int(float64(sampleCount) * ratio)
	result := make([]byte, outputSamples*2)

	for i := 0; i < outputSamples; i++ {
		srcIndex := float64(i) / ratio
		srcIdx := int(srcIndex)
		frac := srcIndex - float64(srcIdx)

		var sample int16
		if srcIdx+1 < sampleCount {
			s0 := int16(binary.LittleEndian.Uint16(pcm[srcIdx*2 : srcIdx*2+2]))
			s1 := int16(binary.LittleEndian.Uint16(pcm[(srcIdx+1)*2 : (srcIdx+1)*2+2]))
			sample = int16(float64(s0)*(1-frac) + float64(s1)*frac)
		} else if srcIdx < sampleCount {
			sample = int16(binary.LittleEndian.Uint16(pcm[srcIdx*2 : srcIdx*2+2]))
		}

		binary.LittleEndian.PutUint16(result[i*2:i*2+2], uint16(sample))
	}

	return result, nil
}

// SplitPCMToChunks splits PCM data into fixed-duration chunks (in milliseconds).
func SplitPCMToChunks(pcm []byte, sampleRate, chunkMs int) ([][]byte, error) {
	if len(pcm)%2 != 0 {
		return nil, fmt.Errorf("PCM data must be 16-bit (even byte length)")
	}

	bytesPerMs := (sampleRate * 2) / 1000 // 2 bytes per sample
	chunkSize := bytesPerMs * chunkMs

	if chunkSize <= 0 {
		return nil, fmt.Errorf("invalid chunk size for rate=%d, ms=%d", sampleRate, chunkMs)
	}

	var chunks [][]byte
	for offset := 0; offset < len(pcm); offset += chunkSize {
		end := offset + chunkSize
		if end > len(pcm) {
			end = len(pcm)
		}
		chunk := make([]byte, end-offset)
		copy(chunk, pcm[offset:end])
		chunks = append(chunks, chunk)
	}
	return chunks, nil
}

func PCMToWAV(pcmData []byte, sampleRate int) []byte {
	dataLen := len(pcmData)
	wavLen := 44 + dataLen

	wav := make([]byte, wavLen)

	copy(wav[0:4], []byte("RIFF"))
	putU32LE(wav[4:8], uint32(wavLen-8))
	copy(wav[8:16], []byte("WAVEfmt "))
	putU32LE(wav[16:20], 16)
	putU16LE(wav[20:22], 1)
	putU16LE(wav[22:24], 1)
	putU32LE(wav[24:28], uint32(sampleRate))
	putU32LE(wav[28:32], uint32(sampleRate*2))
	putU16LE(wav[32:34], 2)
	putU16LE(wav[34:36], 16)
	copy(wav[36:40], []byte("data"))
	putU32LE(wav[40:44], uint32(dataLen))
	copy(wav[44:], pcmData)

	return wav
}

func putU32LE(b []byte, v uint32) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
	b[2] = byte(v >> 16)
	b[3] = byte(v >> 24)
}

func putU16LE(b []byte, v uint16) {
	b[0] = byte(v)
	b[1] = byte(v >> 8)
}

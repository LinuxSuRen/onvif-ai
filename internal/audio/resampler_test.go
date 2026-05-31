package audio

import (
	"encoding/binary"
	"testing"
)

func TestResampleSameRate(t *testing.T) {
	input := make([]byte, 320)
	result, err := ResamplePCM(input, 8000, 8000)
	if err != nil {
		t.Fatalf("same rate resample failed: %v", err)
	}
	if len(result) != 320 {
		t.Fatalf("expected 320 bytes, got %d", len(result))
	}
}

func TestResampleUp(t *testing.T) {
	input := make([]byte, 320)
	for i := 0; i < 160; i++ {
		binary.LittleEndian.PutUint16(input[i*2:i*2+2], uint16(int16(i*100)))
	}
	result, err := ResamplePCM(input, 8000, 16000)
	if err != nil {
		t.Fatalf("upsample failed: %v", err)
	}
	if len(result) != 640 {
		t.Fatalf("expected 640 bytes for 2x upsampling, got %d", len(result))
	}
}

func TestResampleDown(t *testing.T) {
	input := make([]byte, 640)
	for i := 0; i < 320; i++ {
		binary.LittleEndian.PutUint16(input[i*2:i*2+2], uint16(int16(i*50)))
	}
	result, err := ResamplePCM(input, 16000, 8000)
	if err != nil {
		t.Fatalf("downsample failed: %v", err)
	}
	if len(result) != 320 {
		t.Fatalf("expected 320 bytes for 2x downsampling, got %d", len(result))
	}
}

func TestResampleInvalidInput(t *testing.T) {
	_, err := ResamplePCM([]byte{0x00}, 8000, 16000)
	if err == nil {
		t.Fatal("expected error for odd-length input")
	}
}

func TestSplitPCMToChunks(t *testing.T) {
	pcm := make([]byte, 3200)
	chunks, err := SplitPCMToChunks(pcm, 8000, 20)
	if err != nil {
		t.Fatalf("split failed: %v", err)
	}
	if len(chunks) != 10 {
		t.Fatalf("expected 10 chunks for 100ms @ 8kHz, got %d", len(chunks))
	}
	for i, chunk := range chunks {
		if len(chunk) != 320 {
			t.Errorf("chunk %d: expected 320 bytes, got %d", i, len(chunk))
		}
	}
}

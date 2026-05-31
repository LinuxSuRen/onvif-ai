package audio

import (
	"encoding/binary"
	"testing"
)

func TestEncodeDecodeMuLaw(t *testing.T) {
	input := make([]byte, 320)
	for i := 0; i < 160; i++ {
		binary.LittleEndian.PutUint16(input[i*2:i*2+2], uint16(int16(i*100-8000)))
	}

	encoded, err := EncodePCMToG711(input, G711MuLaw)
	if err != nil {
		t.Fatalf("encode failed: %v", err)
	}
	if len(encoded) != 160 {
		t.Fatalf("expected 160 bytes, got %d", len(encoded))
	}

	decoded := DecodeG711ToPCM(encoded, G711MuLaw)
	if len(decoded) != 320 {
		t.Fatalf("expected 320 bytes, got %d", len(decoded))
	}

	for i := 0; i < 160; i++ {
		orig := int16(binary.LittleEndian.Uint16(input[i*2 : i*2+2]))
		dec := int16(binary.LittleEndian.Uint16(decoded[i*2 : i*2+2]))
		diff := orig - dec
		if diff < 0 {
			diff = -diff
		}
		if diff > 4000 {
			t.Errorf("sample %d: original=%d, decoded=%d, diff=%d", i, orig, dec, diff)
		}
	}
}

func TestEncodeDecodeALaw(t *testing.T) {
	input := make([]byte, 320)
	for i := 0; i < 160; i++ {
		binary.LittleEndian.PutUint16(input[i*2:i*2+2], uint16(int16(i*100-8000)))
	}

	encoded, err := EncodePCMToG711(input, G711ALaw)
	if err != nil {
		t.Fatalf("encode failed: %v", err)
	}
	if len(encoded) != 160 {
		t.Fatalf("expected 160 bytes, got %d", len(encoded))
	}

	decoded := DecodeG711ToPCM(encoded, G711ALaw)
	if len(decoded) != 320 {
		t.Fatalf("expected 320 bytes, got %d", len(decoded))
	}
}

func TestEncodeOddLength(t *testing.T) {
	_, err := EncodePCMToG711([]byte{0x00}, G711MuLaw)
	if err == nil {
		t.Fatal("expected error for odd-length PCM")
	}
}

func TestSilenceEncodeDecode(t *testing.T) {
	silence := make([]byte, 320)
	encoded, err := EncodePCMToG711(silence, G711MuLaw)
	if err != nil {
		t.Fatalf("silence encode failed: %v", err)
	}
	if len(encoded) != 160 {
		t.Fatalf("expected 160 bytes for silence, got %d", len(encoded))
	}
	decoded := DecodeG711ToPCM(encoded, G711MuLaw)
	if len(decoded) != 320 {
		t.Fatalf("expected 320 bytes decoded silence, got %d", len(decoded))
	}
}

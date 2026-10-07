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

	for i := 0; i < 160; i++ {
		orig := int16(binary.LittleEndian.Uint16(input[i*2 : i*2+2]))
		dec := int16(binary.LittleEndian.Uint16(decoded[i*2 : i*2+2]))
		diff := orig - dec
		if diff < 0 {
			diff = -diff
		}
		// A-law 分段量化 + 13 位截断的最大误差约半个分段步长
		if diff > 600 {
			t.Errorf("sample %d: original=%d, decoded=%d, diff=%d", i, orig, dec, diff)
		}
	}
}

// TestALawStandardBytes 校验若干参考值与 ITU-T G.711 标准字节一致，
// 保证与真实摄像头的 A-law 互通（历史实现曾产出非标准字节）。
func TestALawStandardBytes(t *testing.T) {
	cases := []struct {
		pcm int16
		out byte
	}{
		{0, 0xD5},      // 正零
		{-1, 0x55},     // 负最小幅度
		{32767, 0xAA},  // 正满幅（段 7 顶格）
		{-32768, 0x2A}, // 负满幅
	}
	for _, c := range cases {
		enc, err := EncodePCMToG711([]byte{byte(c.pcm), byte(c.pcm >> 8)}, G711ALaw)
		if err != nil {
			t.Fatalf("encode %d failed: %v", c.pcm, err)
		}
		if enc[0] != c.out {
			t.Errorf("pcm %d: expected A-law byte %#02X, got %#02X", c.pcm, c.out, enc[0])
		}
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

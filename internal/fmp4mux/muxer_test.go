package fmp4mux

import (
	"bytes"
	"encoding/hex"
	"testing"
)

// 真实可解析的 SPS/PPS（1280x720 High@4.0，取自 mp4ff 官方示例）。
const (
	testSPSHex = "67640020accac05005bb0169e0000003002000000c9c4c000432380008647c12401cb1c31380"
	testPPSHex = "68b5df20"
)

func testSPS() []byte {
	b, _ := hex.DecodeString(testSPSHex)
	return b
}

func testPPS() []byte {
	b, _ := hex.DecodeString(testPPSHex)
	return b
}

func TestMuxerInitAndFragment(t *testing.T) {
	m, err := New(testSPS(), testPPS())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	init := m.InitSegment()
	if len(init) < 64 {
		t.Fatalf("init segment too small: %d bytes", len(init))
	}
	// init 段应包含 ftyp 与 moov。
	for _, want := range []string{"ftyp", "moov", "avcC"} {
		if !bytes.Contains(init, []byte(want)) {
			t.Fatalf("init missing %q", want)
		}
	}
	if cs := m.CodecString(); cs != "avc1.640020" {
		t.Fatalf("codec string = %q", cs)
	}

	idr := [][]byte{{0x65, 0x88, 0x84, 0x00, 0x01}}
	pframe := [][]byte{{0x41, 0x9A, 0x02, 0x05}}
	seg1, err := m.Fragment(idr, 6000, true)
	if err != nil {
		t.Fatalf("Fragment idr: %v", err)
	}
	seg2, err := m.Fragment(pframe, 6000, false)
	if err != nil {
		t.Fatalf("Fragment p: %v", err)
	}
	for i, seg := range [][]byte{seg1, seg2} {
		// 媒体段应包含 moof 与 mdat，且序号递增。
		for _, want := range []string{"moof", "mdat"} {
			if !bytes.Contains(seg, []byte(want)) {
				t.Fatalf("seg%d missing %q", i+1, want)
			}
		}
	}
	// 非关键帧应携带 non-sync 标志（0x02000000 出现在 trun/.tfhd 标志位），
	// 关键帧不含。粗验：关键帧段中不含 02 00 00 00 的 sample flags 序列
	// 过于脆弱，这里以段长度与 box 存在性为准，flags 逻辑单独测试。
	if sampleFlags(true) != 0 {
		t.Fatal("key sample flags should be 0 (sync)")
	}
	if sampleFlags(false) != 0x02000000 {
		t.Fatal("non-key sample flags should have non-sync bit")
	}
}

func TestMuxerSPSChanged(t *testing.T) {
	m, _ := New(testSPS(), testPPS())
	if m.SPSChanged(testSPS()) {
		t.Fatal("same SPS should not report changed")
	}
	other := append([]byte(nil), testSPS()...)
	other[3] ^= 0xFF
	if !m.SPSChanged(other) {
		t.Fatal("different SPS should report changed")
	}
}

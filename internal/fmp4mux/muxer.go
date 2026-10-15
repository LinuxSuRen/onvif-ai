// Package fmp4mux 把 H.264 访问单元（AU）封装为标准 fMP4 片段：
// init 段（moov/avcC）+ 逐帧媒体段（moof/mdat），供浏览器 MSE 直接
// appendBuffer 播放。
//
// 背景：此前浏览器端用 jmuxer 在 JS 里自制 fMP4，其封装不合规范——硬件解码
// 直接拒绝（黑屏闪烁），软件解码产生绿条伪影；换用服务端标准封装后浏览器
// 走原生 MSE 路径（对齐 robot-platform-ui 的 RTSPtoWeb 模式）。
package fmp4mux

import (
	"bytes"
	"fmt"

	"github.com/Eyevinn/mp4ff/mp4"
)

// Timescale 与 RTP 视频时钟一致（H.264 RTP 为 90kHz），
// 采样时长可直接用 RTP 时间戳差值，无换算损失。
const Timescale = 90000

// Muxer 单路视频的 fMP4 封装器。非并发安全，由所属流的读循环独占调用。
type Muxer struct {
	seq  uint32
	sps  []byte
	pps  []byte
	init []byte
	dts  uint64 // 累计解码时间（ticks）
}

// New 用 SPS/PPS 创建封装器并生成 init 段。
func New(sps, pps []byte) (*Muxer, error) {
	m := &Muxer{sps: append([]byte(nil), sps...), pps: append([]byte(nil), pps...)}
	if err := m.buildInit(); err != nil {
		return nil, err
	}
	return m, nil
}

// SPSChanged 判断参数集是否与封装器当前持有的不同（分辨率切换等场景
// 需要重建 init 段）。
func (m *Muxer) SPSChanged(sps []byte) bool {
	return !bytes.Equal(m.sps, sps)
}

func (m *Muxer) buildInit() error {
	init := mp4.CreateEmptyInit()
	trak := init.AddEmptyTrack(Timescale, "video", "und")
	// includePS=true：avcC 内带完整 SPS/PPS，部分实现对带内缺失更敏感。
	if err := trak.SetAVCDescriptor("avc1", [][]byte{m.sps}, [][]byte{m.pps}, true); err != nil {
		return fmt.Errorf("set avc descriptor: %w", err)
	}
	var buf bytes.Buffer
	if err := init.Encode(&buf); err != nil {
		return fmt.Errorf("encode init: %w", err)
	}
	m.init = buf.Bytes()
	return nil
}

// InitSegment 返回 init 段字节（调用方只读，勿修改）。
func (m *Muxer) InitSegment() []byte { return m.init }

// CodecString 返回 MSE 用的 codec 串（avc1.PPCCLL，取自 SPS 字节）。
func (m *Muxer) CodecString() string {
	if len(m.sps) < 4 {
		return "avc1.42E01E"
	}
	return fmt.Sprintf("avc1.%02X%02X%02X", m.sps[1], m.sps[2], m.sps[3])
}

// Fragment 把一个完整 AU 封装为媒体段。nalus 为带起始码的裸 NAL；
// dur 为本帧时长（ticks，90kHz）；key 为是否关键帧。
// 返回的切片每次调用独立分配，调用方可直接持有。
func (m *Muxer) Fragment(nalus [][]byte, dur uint32, key bool) ([]byte, error) {
	if len(nalus) == 0 {
		return nil, fmt.Errorf("empty AU")
	}
	m.seq++
	frag, err := mp4.CreateFragment(m.seq, 1)
	if err != nil {
		return nil, fmt.Errorf("create fragment: %w", err)
	}
	// AVCC：4 字节长度前缀替代起始码
	var data []byte
	for _, n := range nalus {
		data = append(data, byte(len(n)>>24), byte(len(n)>>16), byte(len(n)>>8), byte(len(n)))
		data = append(data, n...)
	}
	sample := mp4.FullSample{
		// 零 B 帧（zerolatency），CTS=DTS，组合偏移恒 0
		Sample:     mp4.NewSample(sampleFlags(key), dur, uint32(len(data)), 0),
		DecodeTime: m.dts,
		Data:       data,
	}
	frag.AddFullSample(sample)
	m.dts += uint64(dur)

	var buf bytes.Buffer
	if err := frag.Encode(&buf); err != nil {
		return nil, fmt.Errorf("encode fragment: %w", err)
	}
	return buf.Bytes(), nil
}

// sampleFlags：关键帧为同步样本，非关键帧置 non-sync 标志。
func sampleFlags(key bool) uint32 {
	if key {
		return 0x02000000 ^ 0x02000000 // 0：sync sample（sample_is_non_sync_sample=0）
	}
	return 0x02000000 // sample_is_non_sync_sample=1（sample_depends_on=2 依赖前帧）
}

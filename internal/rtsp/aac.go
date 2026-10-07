package rtsp

import (
	"encoding/binary"

	aac "github.com/arabian9ts/aac-go"
	"github.com/arabian9ts/aac-go/adts"
)

// aacSamplingRates 是 ADTS/ASC 公共采样率索引表（ISO/IEC 14496-3）。
var aacSamplingRates = [13]int{
	96000, 88200, 64000, 48000, 44100, 32000, 24000, 22050,
	16000, 12000, 11025, 8000, 7350,
}

// aacSamplingFreqIndex 返回采样率在 ADTS/ASC 索引表中的下标，未收录返回 -1。
func aacSamplingFreqIndex(rate int) int {
	for i, r := range aacSamplingRates {
		if r == rate {
			return i
		}
	}
	return -1
}

// aacADTSFrame 为裸 AAC 访问单元（RFC 3640 的 AU 不带传输层封装）拼上
// ADTS 头——流式解码器按 ADTS 帧边界切分。profile 固定 AAC-LC（AOT-1）。
func aacADTSFrame(au []byte, samplingFreqIndex, channels int) ([]byte, error) {
	header, err := adts.Build(adts.Header{
		MPEGID:                 0, // MPEG-4
		Profile:                1, // AAC-LC（AOT 2 − 1）
		SamplingFrequencyIndex: uint8(samplingFreqIndex),
		ChannelConfiguration:   uint8(channels),
		FrameLength:            adts.HeaderSize + len(au),
		BufferFullness:         adts.VariableBitRateBufferFullness,
	})
	if err != nil {
		return nil, err
	}
	frame := make([]byte, 0, len(header)+len(au))
	frame = append(frame, header...)
	frame = append(frame, au...)
	return frame, nil
}

// aacNewDecoder 创建 AAC 解码器；失败时返回 nil（调用方按"无音频"降级）。
func aacNewDecoder() *aac.Decoder {
	d, err := aac.NewDecoder()
	if err != nil {
		return nil
	}
	return d
}

// pcm16ToBytes 把交错 int16 线性 PCM 转为小端字节序。
func pcm16ToBytes(samples []int16) []byte {
	out := make([]byte, len(samples)*2)
	for i, s := range samples {
		binary.LittleEndian.PutUint16(out[i*2:i*2+2], uint16(s))
	}
	return out
}

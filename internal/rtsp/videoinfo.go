package rtsp

import "github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"

// resolutionFromSPS 从 H.264 SPS NAL（含 NAL 头字节）解析像素分辨率。
// 宽高的宏块/裁剪换算由 mediacommon 的 h264.SPS.Width()/Height() 完成。
// 解析失败或尺寸不合理时返回 0。
func resolutionFromSPS(sps []byte) (int, int) {
	if len(sps) < 4 {
		return 0, 0
	}
	var parsed h264.SPS
	if err := parsed.Unmarshal(sps); err != nil {
		return 0, 0
	}
	w, h := parsed.Width(), parsed.Height()
	if w <= 0 || h <= 0 || w > 8192 || h > 8192 {
		return 0, 0
	}
	return w, h
}

// JPEGResolution 扫描 JPEG 字节流的 SOF 段解析分辨率（RTSP MJPEG 与
// HTTP 快照共用）。SOF 段布局：FF Cn | 段长(2) | 精度(1) | 高(2) | 宽(2)。
// 解析失败或尺寸不合理时返回 0。
func JPEGResolution(img []byte) (int, int) {
	// SOI 校验
	if len(img) < 4 || img[0] != 0xFF || img[1] != 0xD8 {
		return 0, 0
	}

	i := 2
	for i+4 <= len(img) {
		if img[i] != 0xFF {
			return 0, 0 // 段序列错乱，放弃
		}
		marker := img[i+1]

		// 无长度字段的独立标记
		if marker == 0xD8 || marker == 0x01 || (marker >= 0xD0 && marker <= 0xD7) {
			i += 2
			continue
		}
		// SOS 之后是熵编码数据，不再有可靠段结构
		if marker == 0xDA {
			return 0, 0
		}

		segLen := int(img[i+2])<<8 | int(img[i+3])
		if segLen < 2 {
			return 0, 0
		}

		// SOF0/1/2/3/5/6/7/9/10/11/13/14/15 携带尺寸（C4/C8/C12 是 DHT 等）
		isSOF := (marker >= 0xC0 && marker <= 0xC3) ||
			(marker >= 0xC5 && marker <= 0xC7) ||
			(marker >= 0xC9 && marker <= 0xCB) ||
			(marker >= 0xCD && marker <= 0xCF)
		if isSOF {
			if i+9 > len(img) {
				return 0, 0
			}
			h := int(img[i+5])<<8 | int(img[i+6])
			w := int(img[i+7])<<8 | int(img[i+8])
			if w <= 0 || h <= 0 || w > 8192 || h > 8192 {
				return 0, 0
			}
			return w, h
		}

		i += 2 + segLen
	}
	return 0, 0
}

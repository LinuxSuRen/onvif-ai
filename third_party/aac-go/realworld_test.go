package aac_test

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	aac "github.com/arabian9ts/aac-go"
	"github.com/arabian9ts/aac-go/adts"
)

// TestDecodeRealWorldVectors decodes committed third-party AAC-LC streams
// (ffmpeg's native encoder with individual tools toggled, plus Apple's
// encoder — see scripts/genvectors.sh) and compares 100 ms RMS envelopes
// against fingerprints of afconvert's reference decode. This keeps `go
// test` hermetic while still exercising short windows, KBD, TNS, M/S,
// intensity, and PNS on real streams; exact SNR gating against the live
// reference decoder lives in scripts/accept.sh.
// TestRejectUnsupportedStreams pins the negative seams with real streams:
// each vector in testdata/realworld/negative must fail with ErrUnsupported
// and a message naming the missing feature — never decode "successfully"
// into wrong audio.
func TestRejectUnsupportedStreams(t *testing.T) {
	cases := []struct {
		name    string
		message string
	}{
		{"heaac_mono", "SBR"},          // HE-AAC: skipping SBR would return lowpassed core audio
		{"surround51", "channel_conf"}, // 5.1: channel_configuration 6
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", "realworld", "negative", tc.name+".aac"))
			if err != nil {
				t.Fatal(err)
			}
			dec, err := aac.NewDecoder()
			if err != nil {
				t.Fatal(err)
			}
			_, err = dec.Decode(raw)
			if !errors.Is(err, aac.ErrUnsupported) {
				t.Fatalf("error = %v, want ErrUnsupported", err)
			}
			if !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("error %q does not name the feature (%q)", err, tc.message)
			}
		})
	}
}

// TestDecodeCRCHeaders rewrites a committed vector's headers to
// protection_absent=0 with dummy CRC words (the decoder skips the CRC
// without verifying) and requires a bit-identical decode to the original —
// real content through the CRC framing path with no extra test data.
func TestDecodeCRCHeaders(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "realworld", "off_mono.aac"))
	if err != nil {
		t.Fatal(err)
	}

	var crcStream []byte
	for off := 0; off < len(raw); {
		h, err := adts.Parse(raw[off:])
		if err != nil {
			t.Fatalf("frame at %d: %v", off, err)
		}
		frame := raw[off : off+h.FrameLength]
		header := append([]byte(nil), frame[:adts.HeaderSize]...)
		header[1] &^= 1 // protection_absent = 0
		newLen := h.FrameLength + adts.CRCSize
		header[3] = header[3]&^0x03 | byte(newLen>>11)
		header[4] = byte(newLen >> 3)
		header[5] = header[5]&^0xe0 | byte(newLen&0x7)<<5
		crcStream = append(crcStream, header...)
		crcStream = append(crcStream, 0xde, 0xad) // dummy CRC, not verified
		crcStream = append(crcStream, frame[adts.HeaderSize:]...)
		off += h.FrameLength
	}

	decode := func(data []byte) []int16 {
		dec, err := aac.NewDecoder()
		if err != nil {
			t.Fatal(err)
		}
		pcm, err := dec.Decode(data)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		return pcm
	}
	want := decode(raw)
	got := decode(crcStream)
	if len(want) != len(got) {
		t.Fatalf("CRC stream decoded %d samples, plain stream %d", len(got), len(want))
	}
	for i := range want {
		if want[i] != got[i] {
			t.Fatalf("sample %d differs: %d vs %d", i, got[i], want[i])
		}
	}
}

func TestDecodeRealWorldVectors(t *testing.T) {
	vectors, err := filepath.Glob(filepath.Join("testdata", "realworld", "*.aac"))
	if err != nil || len(vectors) == 0 {
		t.Fatalf("no committed vectors found: %v", err)
	}
	for _, path := range vectors {
		name := strings.TrimSuffix(filepath.Base(path), ".aac")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var ref struct {
				SampleRate     int       `json:"sampleRate"`
				Channels       int       `json:"channels"`
				SegmentSamples int       `json:"segmentSamples"`
				RMS            []float64 `json:"rms"`
			}
			fp, err := os.ReadFile(filepath.Join("testdata", "realworld", name+".rms.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(fp, &ref); err != nil {
				t.Fatal(err)
			}

			dec, err := aac.NewDecoder()
			if err != nil {
				t.Fatal(err)
			}
			pcm, err := dec.Decode(raw)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if dec.SampleRate() != ref.SampleRate || dec.Channels() != ref.Channels {
				t.Fatalf("format = %d Hz %d ch, want %d Hz %d ch",
					dec.SampleRate(), dec.Channels(), ref.SampleRate, ref.Channels)
			}

			for seg, want := range ref.RMS {
				off := seg * ref.SegmentSamples
				if off+ref.SegmentSamples > len(pcm) {
					break
				}
				var energy float64
				for _, s := range pcm[off : off+ref.SegmentSamples] {
					energy += float64(s) * float64(s)
				}
				got := math.Sqrt(energy / float64(ref.SegmentSamples))
				if want < 100 && got < 100 {
					continue // both near-silent
				}
				if ratio := 20 * math.Log10((got+1)/(want+1)); math.Abs(ratio) > 3 {
					t.Fatalf("segment %d: RMS %.0f vs reference %.0f (%.1f dB apart)", seg, got, want, ratio)
				}
			}
		})
	}
}

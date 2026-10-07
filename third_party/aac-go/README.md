# aac-go

Pure-Go AAC-LC (MPEG-4 Audio, ISO/IEC 14496-3) encoder and decoder. No cgo,
no external binaries.

## Supported features

| Feature | Decoder | Encoder |
|---|:---:|:---:|
| AAC-LC profile (ADTS transport) | ✅ | ✅ |
| Sample rates | ✅ all 13 (8–96 kHz) | ✅ 44.1 / 48 kHz |
| Channels | ✅ mono (SCE), stereo (CPE) | ✅ mono (SCE), stereo (CPE) |
| Long / short windows, block switching | ✅ incl. "meaningless" transitions | ✅ transient detection, 1-block lookahead |
| Window shapes | ✅ sine and KBD | ✅ sine |
| TNS (Temporal Noise Shaping) | ✅ | ⚙️ opt-in `Config.EnableTNS`¹ |
| M/S stereo | ✅ | ❌ |
| Intensity stereo | ✅ | ❌ |
| PNS (Perceptual Noise Substitution) | ✅ | ❌ |
| ADTS CRC headers | ✅ (skipped, not verified) | — (emits CRC-free) |
| PCE (program config element) | ✅ parsed | — |
| Rate control | — | VBR, `Quality` 0–1², 6144-bit/ch frame limit enforced |
| Streaming API (arbitrary chunk sizes) | ✅ | ✅ |

¹ Recommended for transient-dominant material only; degrades tonal content
without a psychoacoustic model.
² Maps to a uniform quantization noise floor; reference decoders recover
55–82 dB SNR across the range on tonal material, strictly monotonic in both
quality and size.

Not supported (explicit `ErrUnsupported` errors, never silent corruption):

| Feature | Notes |
|---|---|
| HE-AAC / HE-AACv2 (SBR, PS) | Detected and rejected — decoding only the core band would sound lowpassed |
| Surround (>2 channels), LFE/CCE | `channel_configuration` 1 and 2 only |
| MP4 / M4A / TS containers | ADTS only; demux upstream |
| Pulse data, 960-sample frames, MAIN/SSR/LTP profiles | Not part of the AAC-LC subset targeted here |

Decoding is verified against streams from ffmpeg's native AAC encoder (each
tool exercised in isolation and combined) and Apple's encoder: deterministic
streams decode within 102–106 dB of the reference decoders; PNS streams
match their RMS envelope (noise is decoder-local by design). The MDCT runs
on an N/4 complex FFT (~5 µs per 2048-point transform) with a direct
evaluation of the standard's formulas kept as the permanent test reference.

## Installation

```sh
go get github.com/arabian9ts/aac-go
```

## Usage

### Encoding

```go
import aac "github.com/arabian9ts/aac-go"

enc, err := aac.NewEncoder(aac.Config{
	SampleRate: 44100, // 44100 or 48000
	Channels:   2,     // 1 (mono) or 2 (stereo)
	Quality:    0.6,   // 0..1, VBR: bitrate/fidelity trade-off
})
if err != nil {
	return err
}

// Input is interleaved int16 PCM. Chunks may have any length; the
// encoder buffers internally and returns completed ADTS frames.
var adts []byte
for chunk := range pcmChunks {
	out, err := enc.Encode(chunk)
	if err != nil {
		return err
	}
	adts = append(adts, out...)
}

// Flush is required: it drains the encoder's lookahead and emits the
// final frames. The encoder is unusable afterwards.
tail, err := enc.Flush()
if err != nil {
	return err
}
adts = append(adts, tail...)
// adts is a playable .aac (ADTS) stream.
```

### Decoding

```go
dec, err := aac.NewDecoder()
if err != nil {
	return err
}

// Input may be sliced at arbitrary byte boundaries (e.g. network
// reads); partial frames are buffered until complete. Each call
// returns the PCM decoded so far, interleaved int16.
var pcm []int16
for chunk := range adtsChunks {
	out, err := dec.Decode(chunk)
	if err != nil {
		return err
	}
	pcm = append(pcm, out...)
}

// The stream format is detected from the first ADTS header.
sampleRate, channels := dec.SampleRate(), dec.Channels()
```

### Error handling

Streams using features outside the supported subset (HE-AAC/SBR,
surround, ...) fail with a wrapped `aac.ErrUnsupported` naming the
feature — never silently wrong audio:

```go
if _, err := dec.Decode(data); errors.Is(err, aac.ErrUnsupported) {
	// e.g. "AAC feature is not supported: HE-AAC/SBR extension ..."
}
```

### Notes

- The codec delay is `aac.SamplesPerFrame` (1024) samples: the decoded
  stream starts with 1024 samples of windowed lead-in per channel, and
  the tail is zero-padded to a frame boundary. Align by this offset when
  comparing against the source.
- Encoder and decoder are not safe for concurrent use; use one instance
  per stream (instances are cheap).
- A minimal 16-bit PCM WAV reader/writer ships as the
  `github.com/arabian9ts/aac-go/wav` subpackage for tooling and tests.

### CLI

```sh
go run ./cmd/aacenc -quality 0.6 input.wav output.aac
go run ./cmd/aacdec output.aac roundtrip.wav
```

## Testing

Three layers, each one command (CI runs exactly these — see
`.github/workflows/ci.yml`, which is a thin wrapper any CI system can copy):

1. `go test ./...` — hermetic; no external tools. Unit tests, synthetic
   roundtrips, and decoding of all committed real-world vectors
   (`testdata/realworld`: ffmpeg's encoder with each tool isolated, every
   sample rate, bitrate extremes, Apple's encoder, plus negative vectors
   that must fail with named errors) checked against committed RMS
   fingerprints. Runs in seconds.
2. `scripts/accept.sh` — verification against a **test oracle**: an
   independent reference decoder used to judge correctness — ffmpeg by
   default, afconvert as an optional local macOS cross-check
   (`ORACLE=afconvert`). Both directions: our encoder's streams must be
   accepted with SNR gates, and our decoder must match the oracle's decode
   of the real-world vectors. The oracle is an external CLI invoked only
   during verification — the library itself has zero dependencies.
3. `go test -fuzz=FuzzDecode -fuzztime 60s .` — time-boxed robustness
   exploration; found crashers are committed as regression seeds.

Test vectors are immutable once committed; `scripts/genvectors.sh` is the
local curation tool that created them (generator versions recorded in its
header) and is never run in CI.

## License

Apache License 2.0 — see [LICENSE](LICENSE).

AAC is a codec domain with a patent history. The Apache-2.0 license
includes an express patent grant covering contributions to this project;
it cannot and does not cover third-party patents. Evaluate your own
patent position for commercial use.

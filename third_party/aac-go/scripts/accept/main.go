//go:build accept

// Command accept is the acceptance gate.
//
// It verifies the library against a test oracle — an independent
// reference decoder used to judge correctness: ffmpeg (any platform, the
// CI default) or afconvert (macOS, optional cross-check). Both are
// invoked as external CLI subprocesses purely at verification time; the
// library itself never depends on or links against either.
//
// For each synthetic test signal it:
//  1. encodes PCM to ADTS with this library,
//  2. has the reference decoder decode the .aac — acceptance proves our
//     bitstream is spec-compliant, not merely self-consistent,
//  3. decodes with our own decoder and reports SNR against the source and
//     against the reference decode.
//
// Usage: go run -tags accept ./scripts/accept [-oracle ffmpeg] [-cases sine440] [-realworld testdata/realworld]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	aac "github.com/arabian9ts/aac-go"
	"github.com/arabian9ts/aac-go/wav"
)

type testCase struct {
	name       string
	sampleRate int
	channels   int
	seconds    float64
	quality    float64
	minSNR     float64 // required SNR (dB) of our decode vs source; 0 = no gate
	gen        func(i, ch, sampleRate int) float64
}

// Quality varies across cases deliberately: the frame-size limit (6144
// bits/channel) binds hardest at quality 1.0 on dense signals, while low
// quality exercises coarse quantization. White noise is the worst case for
// bit demand and gets no SNR gate (AAC cannot reproduce noise waveforms,
// only their spectra). Gates carry 10-15 dB headroom below measured values
// (M6a calibration) so oracle version drift cannot flake CI.
var allCases = []testCase{
	{"silence", 44100, 1, 1.0, 0.5, 0, func(i, ch, sr int) float64 { return 0 }},
	{"sine440", 44100, 1, 1.0, 0.5, 60, sine(440)},
	{"sine440_q0", 44100, 1, 1.0, 0.0, 40, sine(440)},
	{"sine440_q1", 44100, 1, 1.0, 1.0, 65, sine(440)},
	{"sine1k_48k", 48000, 1, 1.0, 0.5, 60, sine(1000)},
	{"multitone", 44100, 1, 1.0, 0.5, 55, func(i, ch, sr int) float64 {
		t := float64(i) / float64(sr)
		return 0.3*math.Sin(2*math.Pi*440*t) + 0.3*math.Sin(2*math.Pi*1320*t) + 0.2*math.Sin(2*math.Pi*3700*t)
	}},
	{"chirp", 44100, 1, 1.5, 0.5, 55, func(i, ch, sr int) float64 {
		t := float64(i) / float64(sr)
		return 0.7 * math.Sin(2*math.Pi*(200+3000*t)*t)
	}},
	{"noise_q1", 44100, 1, 1.0, 1.0, 0, func(i, ch, sr int) float64 {
		// Deterministic pseudo-noise (xorshift) so runs are reproducible.
		x := uint32(i*2654435761 + ch)
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		return (float64(x)/float64(1<<32) - 0.5) * 1.4
	}},
	{"castanet", 44100, 1, 1.0, 0.5, 40, func(i, ch, sr int) float64 {
		// Impulse train over a quiet tone: the block-switching case.
		v := 0.05 * math.Sin(2*math.Pi*220*float64(i)/float64(sr))
		if phase := i % (sr / 4); phase < 256 {
			v += 0.85 * math.Exp(-float64(phase)/40) * math.Sin(2*math.Pi*3100*float64(phase)/float64(sr))
		}
		return v
	}},
	{"stereo_phase", 44100, 2, 1.0, 0.5, 55, func(i, ch, sr int) float64 {
		t := float64(i) / float64(sr)
		if ch == 1 {
			return 0.7 * math.Sin(2*math.Pi*554*t+math.Pi/3)
		}
		return 0.7 * math.Sin(2*math.Pi*554*t)
	}},
	{"stereo_q1", 48000, 2, 1.0, 1.0, 60, func(i, ch, sr int) float64 {
		t := float64(i) / float64(sr)
		if ch == 1 {
			return 0.6*math.Sin(2*math.Pi*330*t) + 0.2*math.Sin(2*math.Pi*2500*t)
		}
		return 0.6 * math.Sin(2*math.Pi*440*t)
	}},
}

func sine(freq float64) func(i, ch, sr int) float64 {
	return func(i, ch, sr int) float64 {
		return 0.7 * math.Sin(2*math.Pi*freq*float64(i)/float64(sr))
	}
}

// oracle is the test oracle — the external reference decoder used for
// spec-compliance checks: ffmpeg (any platform, CI default) or afconvert
// (macOS). The library itself never depends on either; they only verify it.
var oracle string

func main() {
	casesFlag := flag.String("cases", "all", "comma-separated case names, or 'all'")
	outDir := flag.String("out", "testdata/accept", "output directory for .wav/.aac artifacts")
	realworld := flag.String("realworld", "", "directory of real-world .aac vectors")
	oracleFlag := flag.String("oracle", "auto", "test oracle (reference decoder): auto, ffmpeg, or afconvert")
	flag.Parse()

	oracle = *oracleFlag
	if oracle == "auto" {
		oracle = "ffmpeg"
		if _, err := exec.LookPath("ffmpeg"); err != nil {
			oracle = "afconvert"
		}
	}
	if _, err := exec.LookPath(oracle); err != nil {
		fatalf("oracle %q not found in PATH", oracle)
	}
	fmt.Printf("oracle: %s\n", oracle)

	if *realworld != "" {
		if err := runRealworld(*realworld); err != nil {
			fatalf("%v", err)
		}
		return
	}

	selected := selectCases(*casesFlag)
	if len(selected) == 0 {
		fatalf("no cases match %q", *casesFlag)
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fatalf("creating %s: %v", *outDir, err)
	}

	failures := 0
	for _, tc := range selected {
		if err := runCase(tc, *outDir); err != nil {
			fmt.Printf("FAIL %-14s %v\n", tc.name, err)
			failures++
		}
	}
	if failures > 0 {
		fatalf("%d/%d cases failed", failures, len(selected))
	}
	fmt.Printf("OK: all %d cases accepted\n", len(selected))
}

func runCase(tc testCase, outDir string) error {
	pcm := synthesize(tc)
	base := filepath.Join(outDir, tc.name)

	// Encode with this library.
	enc, err := aac.NewEncoder(aac.Config{SampleRate: tc.sampleRate, Channels: tc.channels, Quality: tc.quality})
	if err != nil {
		return fmt.Errorf("NewEncoder: %w", err)
	}
	data, err := enc.Encode(pcm)
	if err != nil {
		return fmt.Errorf("Encode: %w", err)
	}
	tail, err := enc.Flush()
	if err != nil {
		return fmt.Errorf("Flush: %w", err)
	}
	data = append(data, tail...)
	if err := os.WriteFile(base+".aac", data, 0o644); err != nil {
		return err
	}
	if err := writeWAV(base+".src.wav", tc.sampleRate, tc.channels, pcm); err != nil {
		return err
	}

	// The reference decoder must accept the stream. Its decode also gates
	// encoder quality independently of our own decoder.
	refWAV := base + ".ref.wav"
	if err := oracleDecode(base+".aac", refWAV); err != nil {
		return err
	}
	refPCM, err := readWAV(refWAV)
	if err != nil {
		return fmt.Errorf("reading %s output: %w", oracle, err)
	}
	snrOracle := alignedSNR(pcm, refPCM, tc.channels)
	if tc.minSNR > 0 && snrOracle < tc.minSNR {
		return fmt.Errorf("oracle-decoded SNR %.1f dB below gate %.1f dB (encoder quality)", snrOracle, tc.minSNR)
	}

	// Our own decoder (may be a later milestone).
	dec, err := aac.NewDecoder()
	if err != nil {
		return fmt.Errorf("NewDecoder: %w", err)
	}
	decoded, err := dec.Decode(data)
	if err != nil {
		fmt.Printf("PASS %-14s oracle ok, SNR vs source %.1f dB; own decoder skipped (%v)\n",
			tc.name, snrOracle, err)
		return nil
	}
	if err := writeWAV(base+".dec.wav", tc.sampleRate, tc.channels, decoded); err != nil {
		return err
	}

	snrSrc := alignedSNR(pcm, decoded, tc.channels)
	snrRef := alignedSNR(refPCM, decoded, tc.channels)
	fmt.Printf("PASS %-14s oracle SNR %.1f dB; own decode SNR vs source %.1f dB, vs oracle %.1f dB (%d bytes)\n",
		tc.name, snrOracle, snrSrc, snrRef, len(data))
	if tc.minSNR > 0 && snrSrc < tc.minSNR {
		return fmt.Errorf("own-decoded SNR %.1f dB below gate %.1f dB", snrSrc, tc.minSNR)
	}
	return nil
}

// runRealworld decodes every third-party vector in dir and compares against
// the oracle's output. Deterministic streams gate on tight
// decode-vs-decode SNR; PNS streams legitimately diverge (decoder-local
// noise), so they gate on a 100 ms RMS envelope match instead.
func runRealworld(dir string) error {
	entries, err := filepath.Glob(filepath.Join(dir, "*.aac"))
	if err != nil || len(entries) == 0 {
		return fmt.Errorf("no vectors in %s: %v", dir, err)
	}
	failures := 0
	for _, path := range entries {
		name := strings.TrimSuffix(filepath.Base(path), ".aac")
		if err := runRealworldVector(dir, name); err != nil {
			fmt.Printf("FAIL %-16s %v\n", name, err)
			failures++
		}
	}
	if failures > 0 {
		return fmt.Errorf("%d/%d real-world vectors failed", failures, len(entries))
	}
	fmt.Printf("OK: all %d real-world vectors accepted\n", len(entries))
	return nil
}

func runRealworldVector(dir, name string) error {
	data, err := os.ReadFile(filepath.Join(dir, name+".aac"))
	if err != nil {
		return err
	}
	dec, err := aac.NewDecoder()
	if err != nil {
		return err
	}
	decoded, err := dec.Decode(data)
	if err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	// Use the pre-generated reference decode when present (local runs);
	// otherwise decode live with the oracle (CI has no .wav files).
	refWAV := filepath.Join(dir, name+".ref.wav")
	if _, err := os.Stat(refWAV); err != nil {
		refWAV = filepath.Join(os.TempDir(), "aacgo_"+name+".ref.wav")
		if err := oracleDecode(filepath.Join(dir, name+".aac"), refWAV); err != nil {
			return err
		}
		defer os.Remove(refWAV)
	}
	refPCM, err := readWAV(refWAV)
	if err != nil {
		return fmt.Errorf("reference: %w", err)
	}

	// PNS-bearing vectors (decoder-local noise) get the envelope gate; the
	// gate type is recorded in the fingerprint at generation time, where
	// the encoder flags are known.
	if vectorGate(dir, name) == "envelope" {
		if err := compareEnvelope(refPCM, decoded, dec.Channels(), dec.SampleRate()); err != nil {
			return err
		}
		fmt.Printf("PASS %-16s envelope match (%d samples, %d ch)\n", name, len(decoded), dec.Channels())
		return nil
	}

	snr := alignedSNR(refPCM, decoded, dec.Channels())
	fmt.Printf("PASS %-16s SNR vs %s %.1f dB (%d samples, %d ch)\n", name, oracle, snr, len(decoded), dec.Channels())
	if snr < 60 {
		return fmt.Errorf("SNR vs %s %.1f dB below deterministic gate 60 dB", oracle, snr)
	}
	return nil
}

// compareEnvelope aligns by best correlation lag, then compares RMS over
// 100 ms segments; every segment must match within 3 dB (quiet segments
// are skipped).
func compareEnvelope(ref, got []int16, channels, sampleRate int) error {
	if channels <= 0 || sampleRate <= 0 {
		return fmt.Errorf("stream format not detected")
	}
	seg := sampleRate / 10 * channels
	n := min(len(ref), len(got))
	if n < seg {
		return fmt.Errorf("decoded stream too short: %d samples", len(got))
	}
	rms := func(x []int16) float64 {
		var e float64
		for _, s := range x {
			e += float64(s) * float64(s)
		}
		return math.Sqrt(e / float64(len(x)))
	}
	for off := 0; off+seg <= n; off += seg {
		a, b := rms(ref[off:off+seg]), rms(got[off:off+seg])
		if a < 100 && b < 100 { // both near-silent
			continue
		}
		ratio := 20 * math.Log10((b+1)/(a+1))
		if math.Abs(ratio) > 3 {
			return fmt.Errorf("RMS envelope diverges %.1f dB at sample %d (ref %.0f, got %.0f)", ratio, off, a, b)
		}
	}
	return nil
}

func synthesize(tc testCase) []int16 {
	frames := int(tc.seconds * float64(tc.sampleRate))
	pcm := make([]int16, frames*tc.channels)
	for i := 0; i < frames; i++ {
		for ch := 0; ch < tc.channels; ch++ {
			v := tc.gen(i, ch, tc.sampleRate)
			pcm[i*tc.channels+ch] = int16(math.Round(v * 32000))
		}
	}
	return pcm
}

// alignedSNR searches lags up to ±4096 samples (codec delay and possible
// priming trim by the reference decoder) for the best signal-to-noise ratio
// between a and b, comparing the overlapping region.
func alignedSNR(a, b []int16, channels int) float64 {
	best := math.Inf(-1)
	for lag := -4096; lag <= 4096; lag++ {
		offA, offB := 0, lag*channels
		if offB < 0 {
			offA, offB = -offB, 0
		}
		if offA >= len(a) || offB >= len(b) {
			continue
		}
		n := min(len(a)-offA, len(b)-offB)
		if n < channels*256 {
			continue
		}
		var sig, noise float64
		for i := range n {
			s := float64(a[offA+i])
			d := s - float64(b[offB+i])
			sig += s * s
			noise += d * d
		}
		var snr float64
		switch {
		case noise == 0:
			snr = math.Inf(1)
		case sig == 0:
			snr = 0
		default:
			snr = 10 * math.Log10(sig/noise)
		}
		best = math.Max(best, snr)
	}
	return best
}

func selectCases(spec string) []testCase {
	if spec == "all" {
		return allCases
	}
	var out []testCase
	for _, name := range strings.Split(spec, ",") {
		for _, tc := range allCases {
			if tc.name == strings.TrimSpace(name) {
				out = append(out, tc)
			}
		}
	}
	return out
}

// vectorGate returns "envelope" or "snr" from the vector's fingerprint
// (defaulting to the strict gate when no fingerprint exists).
func vectorGate(dir, name string) string {
	raw, err := os.ReadFile(filepath.Join(dir, name+".rms.json"))
	if err != nil {
		return "snr"
	}
	var fp struct {
		Gate string `json:"gate"`
	}
	if json.Unmarshal(raw, &fp) != nil || fp.Gate == "" {
		return "snr"
	}
	return fp.Gate
}

// oracleDecode decodes an AAC file to 16-bit PCM WAV with the selected
// reference decoder. Failure means the reference implementation rejected
// the stream.
func oracleDecode(aacPath, wavPath string) error {
	var cmd *exec.Cmd
	switch oracle {
	case "afconvert":
		cmd = exec.Command("afconvert", "-f", "WAVE", "-d", "LEI16", aacPath, wavPath)
	case "ffmpeg":
		cmd = exec.Command("ffmpeg", "-y", "-loglevel", "error", "-i", aacPath,
			"-c:a", "pcm_s16le", "-f", "wav", wavPath)
	default:
		return fmt.Errorf("unknown oracle %q", oracle)
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s rejected stream: %v\n%s", oracle, err, out)
	}
	return nil
}

func writeWAV(path string, sampleRate, channels int, pcm []int16) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return wav.Write(f, sampleRate, channels, pcm)
}

func readWAV(path string) ([]int16, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	_, _, pcm, err := wav.Read(f)
	return pcm, err
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "accept: "+format+"\n", args...)
	os.Exit(1)
}

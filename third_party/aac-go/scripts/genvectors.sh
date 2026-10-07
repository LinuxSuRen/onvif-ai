#!/usr/bin/env bash
# Curate the committed real-world AAC-LC test vectors.
#
# LOCAL CURATION TOOL — never run in CI. Committed .aac bytes are immutable
# test inputs; regenerating them with a different ffmpeg version changes the
# streams under test. CI verifies the committed vectors, it never recreates
# them. Record the generator versions below when adding vectors.
#
# Requires: ffmpeg (encoder + fingerprint decoder), afconvert (macOS, for
# the Apple-encoder vectors and local .ref.wav files).
#
# Matrix design:
#   - one vector per encoder tool in isolation (-aac_ms/tns/pns/is), plus
#     all-off (window switching only) and encoder defaults (all-on)
#   - one vector per supported sample rate (ffmpeg rejects some; skipped
#     rates are covered by table-consistency unit tests instead)
#   - bitrate extremes (starved -> heavy PNS/IS; generous)
#   - Apple's encoder as a second independent producer
#   - negative/: streams that must fail with a named error (HE-AAC, 5.1)
set -euo pipefail
cd "$(dirname "$0")/.."
OUT=testdata/realworld
mkdir -p "$OUT" "$OUT/negative"

echo "# generator: $(ffmpeg -version 2>/dev/null | head -1)"
command -v afconvert >/dev/null && echo "# generator: afconvert ($(sw_vers -productVersion 2>/dev/null || echo unknown))"

# Program material with transients (tremolo) to force window switching,
# plus tonal + noise content so every tool has something to act on.
SRC_MONO="sine=frequency=440:duration=2,tremolo=f=8:d=0.9"
SRC_STEREO="anoisesrc=color=pink:duration=2:seed=7[n];sine=frequency=520:duration=2,tremolo=f=6:d=0.8[s];[n][s]amerge=inputs=2,pan=stereo|c0<c0+c1|c1<c1"

enc() { # name channels filtergraph rate bitrate extra-flags...
  local name=$1 ch=$2 graph=$3 rate=$4 br=$5; shift 5
  if [ -f "$OUT/$name.aac" ]; then
    echo "KEEP $name (committed vectors are immutable)"
    return 0
  fi
  if ! ffmpeg -y -loglevel error -f lavfi -i "$graph" -ar "$rate" -ac "$ch" \
      -c:a aac -b:a "$br" "$@" -f adts "$OUT/$name.aac" 2>/dev/null; then
    echo "SKIP $name (ffmpeg rejected rate/config)"
    return 0
  fi
  echo "$name: $(stat -f%z "$OUT/$name.aac" 2>/dev/null || stat -c%s "$OUT/$name.aac") bytes"
}

ALL_OFF=(-aac_ms 0 -aac_tns 0 -aac_pns 0 -aac_is 0)

# --- tool axis ---
enc off_mono       1 "$SRC_MONO"   44100  96k "${ALL_OFF[@]}"
enc tns_mono       1 "$SRC_MONO"   44100  96k -aac_ms 0 -aac_tns 1 -aac_pns 0 -aac_is 0
enc pns_mono       1 "$SRC_MONO"   44100  48k -aac_ms 0 -aac_tns 0 -aac_pns 1 -aac_is 0
enc default_mono   1 "$SRC_MONO"   44100 128k
enc off_stereo     2 "$SRC_STEREO" 48000 160k "${ALL_OFF[@]}"
enc ms_stereo      2 "$SRC_STEREO" 48000 160k -aac_ms 1 -aac_tns 0 -aac_pns 0 -aac_is 0
enc is_stereo      2 "$SRC_STEREO" 44100  64k -aac_ms 0 -aac_tns 0 -aac_pns 0 -aac_is 1
enc tns_stereo     2 "$SRC_STEREO" 48000 160k -aac_ms 0 -aac_tns 1 -aac_pns 0 -aac_is 0
enc default_stereo 2 "$SRC_STEREO" 48000 128k

# --- sample rate axis (encoder defaults; decoder must handle every rate) ---
for rate in 8000 11025 12000 16000 22050 24000 32000 64000 88200 96000; do
  enc "rate${rate}_mono" 1 "$SRC_MONO" "$rate" 48k
done
enc rate16000_stereo 2 "$SRC_STEREO" 16000 64k

# --- bitrate extremes ---
enc lowbr_mono    1 "$SRC_MONO"   44100  24k
enc highbr_stereo 2 "$SRC_STEREO" 48000 320k

# --- Apple encoder (independent producer; macOS only) ---
apple_enc() { # name channels graph format dest [bitrate-args...]
  local name=$1 ch=$2 graph=$3 fmt=$4 dest=$5; shift 5
  if [ -f "$dest" ]; then echo "KEEP $name"; return 0; fi
  ffmpeg -y -loglevel error -f lavfi -i "$graph" -ar 44100 -ac "$ch" "$OUT/_src.wav"
  afconvert -f adts -d "$fmt" "$@" "$OUT/_src.wav" "$dest"
  rm -f "$OUT/_src.wav"
  echo "$name: $(stat -f%z "$dest") bytes"
}
if command -v afconvert >/dev/null; then
  apple_enc apple_mono   1 "$SRC_MONO"   'aac ' "$OUT/apple_mono.aac" -b 96000
  apple_enc apple_stereo 2 "$SRC_STEREO" 'aac ' "$OUT/apple_stereo.aac" -b 96000
  # Negative: HE-AAC must fail with the named SBR error, never decode as
  # lowpassed core audio.
  apple_enc heaac_mono   1 "$SRC_MONO"   aach   "$OUT/negative/heaac_mono.aac"
fi

# Negative: 5.1 surround (channel_configuration 6) must fail with a named error.
if [ ! -f "$OUT/negative/surround51.aac" ]; then
  ffmpeg -y -loglevel error -f lavfi -i "anoisesrc=color=pink:duration=1:seed=3" \
    -ac 6 -ar 48000 -c:a aac -b:a 320k -f adts "$OUT/negative/surround51.aac" \
    && echo "negative/surround51: $(stat -f%z "$OUT/negative/surround51.aac" 2>/dev/null || stat -c%s "$OUT/negative/surround51.aac") bytes"
fi

# --- reference decodes (local .ref.wav, gitignored) + committed fingerprints ---
# Fingerprints use ffmpeg's decode so any CI platform can audit them.
for f in "$OUT"/*.aac; do
  name=$(basename "$f" .aac)
  ffmpeg -y -loglevel error -i "$f" -c:a pcm_s16le -f wav "$OUT/$name.ref.wav"
done

python3 - "$OUT" <<'EOF'
import struct, wave, json, math, glob, os, sys, re
out = sys.argv[1]
# Gate selection: vectors encoded with PNS possible (encoder defaults, or
# -aac_pns 1) legitimately diverge from any reference decoder because noise
# bands are filled from decoder-local PRNGs — those gate on the RMS
# envelope. Vectors from tool-isolated runs without PNS, and Apple's LC
# encoder (no PNS), are deterministic and gate on tight SNR.
ENVELOPE = re.compile(r'^(pns|default|rate|lowbr|highbr)')
for ref in sorted(glob.glob(os.path.join(out, '*.ref.wav'))):
    name = os.path.basename(ref)[:-8]
    w = wave.open(ref)
    ch, n, sr = w.getnchannels(), w.getnframes(), w.getframerate()
    pcm = struct.unpack('<%dh' % (n*ch), w.readframes(n))
    seg = sr // 10 * ch
    rms = []
    for off in range(0, len(pcm) - seg + 1, seg):
        e = sum(float(x)*x for x in pcm[off:off+seg])
        rms.append(round(math.sqrt(e/seg), 1))
    gate = "envelope" if ENVELOPE.match(name) else "snr"
    with open(os.path.join(out, name + '.rms.json'), 'w') as f:
        json.dump({"sampleRate": sr, "channels": ch, "segmentSamples": seg,
                   "gate": gate, "rms": rms}, f)
    print(name, len(rms), "segments,", gate, "gate")
EOF

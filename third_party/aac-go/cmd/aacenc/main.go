// Command aacenc encodes a 16-bit PCM WAV file into an ADTS .aac stream.
//
// Usage: aacenc [-quality 0.5] input.wav output.aac
package main

import (
	"flag"
	"fmt"
	"os"

	aac "github.com/arabian9ts/aac-go"
	"github.com/arabian9ts/aac-go/wav"
)

func main() {
	quality := flag.Float64("quality", 0.5, "encoding quality in [0, 1]; higher is better")
	flag.Parse()
	if flag.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: aacenc [-quality 0.5] input.wav output.aac")
		os.Exit(2)
	}

	if err := run(flag.Arg(0), flag.Arg(1), *quality); err != nil {
		fmt.Fprintln(os.Stderr, "aacenc:", err)
		os.Exit(1)
	}
}

func run(inPath, outPath string, quality float64) error {
	in, err := os.Open(inPath)
	if err != nil {
		return err
	}
	defer in.Close()

	sampleRate, channels, pcm, err := wav.Read(in)
	if err != nil {
		return err
	}

	enc, err := aac.NewEncoder(aac.Config{
		SampleRate: sampleRate,
		Channels:   channels,
		Quality:    quality,
	})
	if err != nil {
		return err
	}
	data, err := enc.Encode(pcm)
	if err != nil {
		return err
	}
	tail, err := enc.Flush()
	if err != nil {
		return err
	}
	data = append(data, tail...)

	if err := os.WriteFile(outPath, data, 0o644); err != nil {
		return err
	}
	seconds := float64(len(pcm)) / float64(sampleRate*channels)
	fmt.Printf("%s: %.2fs %d Hz %dch -> %d ADTS bytes (%.1f kbit/s)\n",
		outPath, seconds, sampleRate, channels, len(data), float64(len(data)*8)/seconds/1000)
	return nil
}

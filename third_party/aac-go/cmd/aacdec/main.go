// Command aacdec decodes an ADTS .aac stream into a 16-bit PCM WAV file.
//
// Usage: aacdec input.aac output.wav
package main

import (
	"fmt"
	"os"

	aac "github.com/arabian9ts/aac-go"
	"github.com/arabian9ts/aac-go/wav"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: aacdec input.aac output.wav")
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "aacdec:", err)
		os.Exit(1)
	}
}

func run(inPath, outPath string) error {
	data, err := os.ReadFile(inPath)
	if err != nil {
		return err
	}

	dec, err := aac.NewDecoder()
	if err != nil {
		return err
	}
	pcm, err := dec.Decode(data)
	if err != nil {
		return err
	}

	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer out.Close()
	if err := wav.Write(out, dec.SampleRate(), dec.Channels(), pcm); err != nil {
		return err
	}
	fmt.Printf("%s: %d samples, %d Hz, %dch\n", outPath, len(pcm)/dec.Channels(), dec.SampleRate(), dec.Channels())
	return nil
}

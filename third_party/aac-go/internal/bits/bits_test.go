package bits

import (
	"errors"
	"io"
	"math/rand/v2"
	"testing"
)

func TestBitRoundTrip(t *testing.T) {
	for seed := range uint64(100) {
		rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
		writer := NewBitWriter()

		type field struct {
			value uint64
			bits  int
		}
		fields := make([]field, 200)
		for i := range fields {
			width := rng.IntN(65)
			value := rng.Uint64()
			if width < 64 {
				value &= (uint64(1) << width) - 1
			}
			fields[i] = field{value: value, bits: width}
			if err := writer.WriteBits(value, width); err != nil {
				t.Fatalf("seed %d, field %d: WriteBits: %v", seed, i, err)
			}
		}

		reader := NewBitReader(writer.Bytes())
		for i, want := range fields {
			got, err := reader.ReadBits(want.bits)
			if err != nil {
				t.Fatalf("seed %d, field %d: ReadBits: %v", seed, i, err)
			}
			if got != want.value {
				t.Fatalf("seed %d, field %d: got %#x, want %#x", seed, i, got, want.value)
			}
		}
	}
}

func TestBitOrderAndAlignment(t *testing.T) {
	w := NewWriter()
	if err := w.WriteBits(0b101, 3); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteBits(0b00111, 5); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteBits(1, 1); err != nil {
		t.Fatal(err)
	}
	w.Align()
	if got, want := w.Bytes(), []byte{0b10100111, 0b10000000}; string(got) != string(want) {
		t.Fatalf("got %08b, want %08b", got, want)
	}
	if got, want := w.Len(), 16; got != want {
		t.Fatalf("Len = %d, want %d", got, want)
	}
}

func TestBitErrors(t *testing.T) {
	w := NewWriter()
	if err := w.WriteBits(8, 3); err == nil {
		t.Fatal("WriteBits accepted an overflowing value")
	}
	if err := w.WriteBits(0, 65); !errors.Is(err, ErrInvalidBitCount) {
		t.Fatalf("WriteBits error = %v, want ErrInvalidBitCount", err)
	}

	r := NewReader([]byte{0})
	if _, err := r.ReadBits(9); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("ReadBits error = %v, want io.ErrUnexpectedEOF", err)
	}
}

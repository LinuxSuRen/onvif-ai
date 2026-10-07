// Package bits implements the MSB-first bit I/O used by AAC syntax elements.
package bits

import (
	"errors"
	"fmt"
	"io"
)

// ErrInvalidBitCount reports a request that cannot fit in a uint64.
var ErrInvalidBitCount = errors.New("bit count must be between 0 and 64")

// BitWriter writes fields most-significant bit first.
type BitWriter struct {
	buf  []byte
	bits int
}

// NewBitWriter returns an empty MSB-first writer.
func NewBitWriter() *BitWriter {
	return &BitWriter{}
}

// NewWriter is a short form of NewBitWriter.
func NewWriter() *BitWriter {
	return NewBitWriter()
}

// WriteBits appends the low n bits of value, most-significant bit first.
func (w *BitWriter) WriteBits(value uint64, n int) error {
	if n < 0 || n > 64 {
		return ErrInvalidBitCount
	}
	if n < 64 && value >= uint64(1)<<n {
		return fmt.Errorf("value %#x does not fit in %d bits", value, n)
	}

	for i := n - 1; i >= 0; i-- {
		if w.bits%8 == 0 {
			w.buf = append(w.buf, 0)
		}
		if value&(uint64(1)<<i) != 0 {
			w.buf[len(w.buf)-1] |= 1 << (7 - w.bits%8)
		}
		w.bits++
	}
	return nil
}

// WriteBit appends one bit.
func (w *BitWriter) WriteBit(value bool) error {
	if value {
		return w.WriteBits(1, 1)
	}
	return w.WriteBits(0, 1)
}

// Align pads with zero bits through the next byte boundary.
func (w *BitWriter) Align() {
	if remainder := w.bits % 8; remainder != 0 {
		w.bits += 8 - remainder
	}
}

// Len returns the number of written bits, including alignment padding.
func (w *BitWriter) Len() int {
	return w.bits
}

// Bytes returns a copy of the encoded bytes. A partial last byte is zero-padded.
func (w *BitWriter) Bytes() []byte {
	return append([]byte(nil), w.buf...)
}

// BitReader reads fields most-significant bit first.
type BitReader struct {
	buf  []byte
	bits int
}

// NewBitReader returns an MSB-first reader over data.
func NewBitReader(data []byte) *BitReader {
	return &BitReader{buf: data}
}

// NewReader is a short form of NewBitReader.
func NewReader(data []byte) *BitReader {
	return NewBitReader(data)
}

// ReadBits returns the next n bits as an unsigned integer.
func (r *BitReader) ReadBits(n int) (uint64, error) {
	if n < 0 || n > 64 {
		return 0, ErrInvalidBitCount
	}
	if n > r.Remaining() {
		return 0, io.ErrUnexpectedEOF
	}

	var value uint64
	for range n {
		value = value<<1 | uint64((r.buf[r.bits/8]>>(7-r.bits%8))&1)
		r.bits++
	}
	return value, nil
}

// ReadBit returns the next bit.
func (r *BitReader) ReadBit() (bool, error) {
	value, err := r.ReadBits(1)
	return value != 0, err
}

// Position returns the number of consumed bits.
func (r *BitReader) Position() int {
	return r.bits
}

// Remaining returns the number of unread bits.
func (r *BitReader) Remaining() int {
	return len(r.buf)*8 - r.bits
}

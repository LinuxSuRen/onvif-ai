// Package transform implements the modified discrete cosine transform pair
// used by AAC (ISO/IEC 14496-3 §4.6.11) together with the window functions
// applied around it.
//
// The fast path evaluates the MDCT through a length-N/4 complex FFT using
// the classic decomposition (fold the N inputs to a length-N/2 DCT-IV, then
// pack the DCT-IV into the FFT with pre/post rotations; the approach goes
// back to Duhamel, Mahieux & Petit, ICASSP 1991). The index mappings and
// rotations below were derived directly from the transform definitions; the
// direct O(N²/2) evaluation of the standard's formulas is kept permanently
// as the test reference, and equivalence_test.go requires the two paths to
// agree on every coefficient.
package transform

import (
	"math"
	"sync"
)

// MDCT holds the precomputed rotations for one transform size.
//
// N is the window length (2048 for an AAC long window, 256 for a short
// window); each transform maps N time samples to N/2 spectral coefficients
// and back.
type MDCT struct {
	n    int
	pre  []complex128 // e^(-iπt/M), t = 0..N/4-1: input packing rotation
	post []complex128 // e^(-iπ(4j+1)/(4M)), j = 0..N/4-1: output rotation
	fft  *fftPlan

	// Reference basis for directForward/directInverse, built lazily: the
	// tests need it, production coding does not.
	basisOnce sync.Once
	basis     [][]float64 // cos[k][i] = cos(2π/N · (i + n0) · (k + ½))
}

// New precomputes the MDCT rotations for window length n (must be a
// multiple of four with n/4 a power of two — both AAC sizes qualify).
func New(n int) *MDCT {
	if n <= 0 || n%4 != 0 {
		panic("transform: MDCT size must be a positive multiple of four")
	}
	m := n / 2
	q := n / 4
	mdct := &MDCT{n: n, fft: newFFTPlan(q)}
	mdct.pre = make([]complex128, q)
	mdct.post = make([]complex128, q)
	for t := range q {
		angle := -math.Pi * float64(t) / float64(m)
		mdct.pre[t] = complex(math.Cos(angle), math.Sin(angle))
	}
	for j := range q {
		angle := -math.Pi * float64(4*j+1) / float64(4*m)
		mdct.post[j] = complex(math.Cos(angle), math.Sin(angle))
	}
	return mdct
}

// dctIV computes the length-M DCT-IV,
//
//	C[k] = Σ_{m=0}^{M-1} v[m] · cos(π/M · (m + ½) · (k + ½)),
//
// through one length-M/2 complex FFT. Packing g[t] = v[2t] + i·v[M-1-2t]
// and rotating by pre[t] turns the even-index kernel outputs into an FFT;
// the odd-index outputs follow from the kernel's k ↦ M-1-k reflection,
// giving C[2j] = Re G[j] and C[M-1-2j] = -Im G[j] with
// G[j] = post[j] · FFT(g·pre)[j].
func (m *MDCT) dctIV(v []float64) []float64 {
	q := m.n / 4
	g := make([]complex128, q)
	for t := range q {
		g[t] = complex(v[2*t], v[len(v)-1-2*t]) * m.pre[t]
	}
	m.fft.transform(g)
	c := make([]float64, len(v))
	for j := range q {
		gj := g[j] * m.post[j]
		c[2*j] = real(gj)
		c[len(v)-1-2*j] = -imag(gj)
	}
	return c
}

// Forward computes the encoder-side MDCT — the analysis filterbank of
// ISO/IEC 14496-3 §4.6.11 with the coefficient chosen so that
// Forward → Inverse → overlap-add reconstructs the input at unity gain:
//
//	X[k] = 2 · Σ_{i=0}^{N-1} z[i] · cos(2π/N · (i + n0) · (k + ½))
//
// z must already be windowed and have length N; the result has length N/2.
func (m *MDCT) Forward(z []float64) []float64 {
	if len(z) != m.n {
		panic("transform: Forward input length must equal window length")
	}
	// Fold the four quarters of z into a length-N/2 sequence whose DCT-IV
	// equals the MDCT kernel sum: with n0 = N/4 + ½, the kernel argument
	// (i + n0)(k + ½) reduces quarter by quarter to ±(m + ½)(k + ½)
	// through the cosine's reflection and periodicity.
	q := m.n / 4
	v := make([]float64, m.n/2)
	for r := range q {
		v[r] = -z[3*q+r] - z[3*q-1-r]
		v[q+r] = z[r] - z[2*q-1-r]
	}
	c := m.dctIV(v)
	for k := range c {
		c[k] *= 2
	}
	return c
}

// Inverse computes the decoder-side IMDCT exactly as defined by
// ISO/IEC 14496-3 §4.6.11.1:
//
//	x[i] = (2/N) · Σ_{k=0}^{N/2-1} X[k] · cos(2π/N · (i + n0) · (k + ½))
//
// spec must have length N/2; the result has length N and must still be
// windowed and overlap-added by the caller.
func (m *MDCT) Inverse(spec []float64) []float64 {
	if len(spec) != m.n/2 {
		panic("transform: Inverse input length must equal half the window length")
	}
	// The synthesis kernel folds identically in the time index, so the
	// output is the spectrum's DCT-IV scattered back through the transpose
	// of the Forward mapping.
	q := m.n / 4
	c := m.dctIV(spec)
	scale := 2 / float64(m.n)
	x := make([]float64, m.n)
	for r := range q {
		x[3*q+r] = -scale * c[r]
		x[3*q-1-r] = -scale * c[r]
		x[r] = scale * c[q+r]
		x[2*q-1-r] = -scale * c[q+r]
	}
	return x
}

// directForward is the permanent reference implementation: a literal
// O(N²/2) evaluation of the analysis formula. The optimized path must
// match it within the equivalence_test.go tolerance.
func (m *MDCT) directForward(z []float64) []float64 {
	if len(z) != m.n {
		panic("transform: Forward input length must equal window length")
	}
	spec := make([]float64, m.n/2)
	for k, row := range m.referenceBasis() {
		var sum float64
		for i, v := range z {
			sum += v * row[i]
		}
		spec[k] = 2 * sum
	}
	return spec
}

// directInverse is the permanent reference implementation of the synthesis
// formula (see directForward).
func (m *MDCT) directInverse(spec []float64) []float64 {
	if len(spec) != m.n/2 {
		panic("transform: Inverse input length must equal half the window length")
	}
	x := make([]float64, m.n)
	scale := 2 / float64(m.n)
	for k, row := range m.referenceBasis() {
		c := spec[k]
		if c == 0 {
			continue
		}
		for i, w := range row {
			x[i] += c * w
		}
	}
	for i := range x {
		x[i] *= scale
	}
	return x
}

// referenceBasis lazily builds the literal cosine basis used only by the
// direct reference implementations.
func (m *MDCT) referenceBasis() [][]float64 {
	m.basisOnce.Do(func() {
		n0 := (float64(m.n)/2 + 1) / 2
		m.basis = make([][]float64, m.n/2)
		for k := range m.basis {
			row := make([]float64, m.n)
			for i := range row {
				row[i] = math.Cos(2 * math.Pi / float64(m.n) * (float64(i) + n0) * (float64(k) + 0.5))
			}
			m.basis[k] = row
		}
	})
	return m.basis
}

// fftPlan is an iterative radix-2 decimation-in-time complex FFT — the
// textbook Cooley-Tukey structure: bit-reversal permutation followed by
// butterfly stages — with twiddle factors precomputed per size.
type fftPlan struct {
	size    int
	rev     []int
	twiddle []complex128 // e^(-i2πt/size), t = 0..size/2-1
}

func newFFTPlan(size int) *fftPlan {
	if size == 0 || size&(size-1) != 0 {
		panic("transform: FFT size must be a power of two")
	}
	p := &fftPlan{size: size, rev: make([]int, size), twiddle: make([]complex128, size/2)}
	bits := 0
	for 1<<bits < size {
		bits++
	}
	for i := range p.rev {
		r := 0
		for b := range bits {
			if i&(1<<b) != 0 {
				r |= 1 << (bits - 1 - b)
			}
		}
		p.rev[i] = r
	}
	for t := range p.twiddle {
		angle := -2 * math.Pi * float64(t) / float64(size)
		p.twiddle[t] = complex(math.Cos(angle), math.Sin(angle))
	}
	return p
}

// transform runs the FFT in place.
func (p *fftPlan) transform(a []complex128) {
	for i, r := range p.rev {
		if i < r {
			a[i], a[r] = a[r], a[i]
		}
	}
	for span := 2; span <= p.size; span *= 2 {
		step := p.size / span
		for start := 0; start < p.size; start += span {
			for k := range span / 2 {
				w := p.twiddle[k*step]
				lo, hi := start+k, start+k+span/2
				even, odd := a[lo], a[hi]*w
				a[lo] = even + odd
				a[hi] = even - odd
			}
		}
	}
}

// SineWindow returns the sine window of length n — ISO/IEC 14496-3 §4.6.11.3
// (window_shape == 0): w[i] = sin(π/N · (i + ½)).
func SineWindow(n int) []float64 {
	w := make([]float64, n)
	for i := range w {
		w[i] = math.Sin(math.Pi / float64(n) * (float64(i) + 0.5))
	}
	return w
}

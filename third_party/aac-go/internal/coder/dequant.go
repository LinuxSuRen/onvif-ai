package coder

import "math"

// ScalefactorOffset is SF_OFFSET from ISO/IEC 14496-3 §4.6.2: the scalefactor
// value at which the dequantizer gain is exactly 1.
const ScalefactorOffset = 100

// Dequantize inverts the AAC nonuniform quantizer for one coefficient —
// ISO/IEC 14496-3 §4.6.2.3:
//
//	x = sign(q) · |q|^(4/3) · 2^((sf − SF_OFFSET)/4)
//
// Callers typically hoist BandGain out of the per-coefficient loop.
func Dequantize(q int32, scalefactor int) float64 {
	return DequantizeWithGain(q, BandGain(scalefactor))
}

// BandGain returns the dequantizer gain 2^((sf − SF_OFFSET)/4) shared by all
// coefficients of a scalefactor band.
func BandGain(scalefactor int) float64 {
	return math.Exp2(float64(scalefactor-ScalefactorOffset) / 4)
}

// DequantizeWithGain applies the |q|^(4/3) magnitude expansion and a
// precomputed band gain.
func DequantizeWithGain(q int32, gain float64) float64 {
	if q == 0 {
		return 0
	}
	magnitude := math.Pow(math.Abs(float64(q)), 4.0/3.0)
	if q < 0 {
		return -magnitude * gain
	}
	return magnitude * gain
}

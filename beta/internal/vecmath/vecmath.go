// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package vecmath

import "math"

// Why the bits come out the same on amd64 and arm64:
//
//   - Each value is widened to float64 before it's multiplied. A float32
//     has 24 bits of precision, so the product of two has at most 48, and a
//     float64 holds it exactly.
//   - The products go into 8 running sums, value i into sum i mod 8, and
//     the sums are combined in one fixed order at the end. Go doesn't
//     reorder floating-point additions, and doesn't flush denormals to zero.
//   - Go may fuse a multiply and an add into one instruction, as it does on
//     arm64. That rounds once instead of twice, but the product is exact, so
//     the sum comes out the same either way.
//   - Addition, multiplication, division and square roots are rounded the
//     same way by both processors, as IEEE 754 requires.
//
// The 8 sums are there for speed: they give the processor 8 additions that
// don't wait on each other. SIMD code added later gives the same bits as
// long as it keeps the same 8 sums and the same combining order.

// Dot returns the dot product of a and b, which have the same length.
func Dot(a, b []float32) float64 {
	b = b[:len(a)]
	var s0, s1, s2, s3, s4, s5, s6, s7 float64
	n := len(a) &^ 7
	for i := 0; i < n; i += 8 {
		x := a[i : i+8 : i+8]
		y := b[i : i+8 : i+8]
		s0 += float64(x[0]) * float64(y[0])
		s1 += float64(x[1]) * float64(y[1])
		s2 += float64(x[2]) * float64(y[2])
		s3 += float64(x[3]) * float64(y[3])
		s4 += float64(x[4]) * float64(y[4])
		s5 += float64(x[5]) * float64(y[5])
		s6 += float64(x[6]) * float64(y[6])
		s7 += float64(x[7]) * float64(y[7])
	}
	var t [8]float64
	for i := n; i < len(a); i++ {
		t[i-n] = float64(a[i]) * float64(b[i])
	}
	return combine(s0+t[0], s1+t[1], s2+t[2], s3+t[3], s4+t[4], s5+t[5], s6+t[6], s7+t[7])
}

// Wide is a query vector widened to float64 once, for comparing with many
// stored vectors. Its values are float32 values, so the products stay
// exact; a Wide made any other way loses the promise of equal bits.
type Wide []float64

// Widen returns q as a Wide.
func Widen(q []float32) Wide {
	w := make(Wide, len(q))
	for i, x := range q {
		w[i] = float64(x)
	}
	return w
}

// Dot returns the dot product of w and v, the same bits as Dot on the
// float32 values w was made from.
func (w Wide) Dot(v []float32) float64 {
	v = v[:len(w)]
	var s0, s1, s2, s3, s4, s5, s6, s7 float64
	n := len(w) &^ 7
	for i := 0; i < n; i += 8 {
		q := w[i : i+8 : i+8]
		x := v[i : i+8 : i+8]
		s0 += q[0] * float64(x[0])
		s1 += q[1] * float64(x[1])
		s2 += q[2] * float64(x[2])
		s3 += q[3] * float64(x[3])
		s4 += q[4] * float64(x[4])
		s5 += q[5] * float64(x[5])
		s6 += q[6] * float64(x[6])
		s7 += q[7] * float64(x[7])
	}
	var t [8]float64
	for i := n; i < len(w); i++ {
		t[i-n] = w[i] * float64(v[i])
	}
	return combine(s0+t[0], s1+t[1], s2+t[2], s3+t[3], s4+t[4], s5+t[5], s6+t[6], s7+t[7])
}

// combine adds the 8 sums in the one order every caller uses.
func combine(s0, s1, s2, s3, s4, s5, s6, s7 float64) float64 {
	return ((s0 + s1) + (s2 + s3)) + ((s4 + s5) + (s6 + s7))
}

// Norm returns a's length, the square root of its dot product with itself.
func Norm(a []float32) float64 { return math.Sqrt(Dot(a, a)) }

// Distance returns the cosine distance from a dot product and the two
// vectors' norms: one minus the cosine, kept between 0 for the same
// direction and 2 for opposite ones, and 1 when either vector is all zeros.
// It's the formula 0.x uses.
func Distance(dot, normA, normB float64) float64 {
	if normA == 0 || normB == 0 {
		return 1
	}
	p := float64(normA * normB) // rounded here, so nothing fuses across it
	d := 1 - dot/p
	switch {
	case d < 0:
		return 0
	case d > 2:
		return 2
	}
	return d
}

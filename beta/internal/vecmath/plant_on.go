// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux && hypercrux_planted

package vecmath

import "os"

// A build with the hypercrux_planted tag can switch on one of this
// package's planted bugs, named in HYPERCRUX_PLANT. scripts/planted.sh runs
// the tests with each bug listed in beta/plants.txt, and every one must
// make them fail.
//
// A name this package doesn't know switches nothing on, so the tests pass
// and the script reports the bug as missed, rather than counting a typo as
// a catch.
var plant = func() string {
	switch p := os.Getenv("HYPERCRUX_PLANT"); p {
	case "vecmath/four-sums", "vecmath/sums-in-a-row":
		return p
	}
	return ""
}()

func plantedDot(a, b []float32) float64 {
	b = b[:len(a)]
	switch plant {
	case "vecmath/four-sums": // 4 running sums instead of 8
		var s [4]float64
		for i := range a {
			s[i%4] += float64(a[i]) * float64(b[i])
		}
		return (s[0] + s[1]) + (s[2] + s[3])
	case "vecmath/sums-in-a-row": // the 8 sums combined left to right
		var s [8]float64
		for i := range a {
			s[i%8] += float64(a[i]) * float64(b[i])
		}
		return s[0] + s[1] + s[2] + s[3] + s[4] + s[5] + s[6] + s[7]
	}
	panic("vecmath: unknown planted bug " + plant)
}

func plantedWideDot(w Wide, v []float32) float64 {
	a := make([]float32, len(w))
	for i, x := range w {
		a[i] = float32(x)
	}
	return plantedDot(a, v)
}

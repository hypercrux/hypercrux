// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux && !hypercrux_planted

package vecmath

// plant is always empty in an ordinary build, so the compiler drops every
// branch that checks it, and no planted bug is built in.
const plant = ""

func plantedDot(a, b []float32) float64 { panic("vecmath: no planted bugs in this build") }

func plantedWideDot(w Wide, v []float32) float64 { panic("vecmath: no planted bugs in this build") }

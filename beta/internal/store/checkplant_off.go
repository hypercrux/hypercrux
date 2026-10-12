// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux && !hypercrux_planted

package store

// checkPlant is always empty in an ordinary build, so the compiler drops
// every branch that checks it, and no planted bug in the check is built in.
// The check's planted bugs have this pair of files of their own, beside
// plant_on.go and plant_off.go (G7).
const checkPlant = ""

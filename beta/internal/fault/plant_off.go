// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux && !hypercrux_planted

package fault

// plant is always empty in an ordinary build, so the compiler drops every
// branch that checks it, and no planted bug is built in.
const plant = ""

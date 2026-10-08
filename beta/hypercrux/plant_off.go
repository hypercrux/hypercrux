// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux && !hypercrux_planted

package hypercrux

// plant is always empty in an ordinary build, so the compiler drops every
// branch that checks it, and no planted bug is built in.
const plant = ""

// shared is only ever called by a planted bug, which an ordinary build
// leaves out. Here it copies, as the driver does.
func shared(s string) []byte { return []byte(s) }

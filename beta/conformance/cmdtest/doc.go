// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package cmdtest holds 0.x's tests of the hypercrux command, run against a
// built binary from outside, so the same tests check 0.x's command and the
// Beta's. The binary is the one HYPERCRUX_BIN names, or 0.x's command, built
// for the test, when it's unset. HYPERCRUX_CMD_SKIP lists sections to leave
// out, separated by commas.
//
// Like the rest of the beta folder it is Linux only; on other systems this
// package is empty.
package cmdtest

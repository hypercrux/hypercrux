// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package cmdtest holds 0.x's tests of the hypercrux command, run against a
// built binary from outside, so the same tests check 0.x's command and the
// Beta's. The binary is the one HYPERCRUX_BIN names. When that's unset, the
// tests build both commands and run on each: 0.x's with every section, and
// the Beta's without the sections the table of named tests in beta/SQL.md
// leaves out. HYPERCRUX_CMD_SKIP lists more sections to leave out, separated
// by commas. TestDrop runs the drop verb on each binary, since the one
// section that runs it is one the Beta leaves out. TestEveryNamedTestExists
// holds the table of named tests, the tests the Beta must pass, to this
// suite and the conformance suite.
//
// Like the rest of the beta folder it is Linux only; on other systems this
// package is empty.
package cmdtest

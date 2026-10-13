// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux && hypercrux_planted

package cmdtest

// betaTags are the build tags the Beta's command is built with for the
// tests. With the hypercrux_planted tag on these tests, as scripts/planted.sh
// builds them, the command has it too, so a bug planted in the command,
// which HYPERCRUX_PLANT switches on in each run of it, is there for these
// tests to catch.
const betaTags = "hypercrux_planted"

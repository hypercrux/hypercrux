// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package procs

import "testing"

// The tests run workloads in copies of the test binary, which Main sends
// to their roles, so every workload a test runs is here.
func TestMain(m *testing.M) {
	Main(m, toy, toyMarkerFirst, toyWithoutTheLock, realLog, realFollowed, realReloaded, realReloadedSlowly,
		failingWriter, panickingWriter, exitingWriter, racyWriter, quittingWriter, skippingReader, stuckReader, pipeHolder, idleWriter, countingReader)
}

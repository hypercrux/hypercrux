// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package procs

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// TestTheToyLogPasses is the half of T4's closing test where the toy log
// has no bug. Two writers and two readers run on it for 2 seconds, or half
// a second with -short, each killed with SIGKILL at a random moment in a
// life of 10 to 200 milliseconds and a new one started in its place. Every
// reader sees every commit once and in order, the readers agree with each
// other and with the file at the end, every commit a writer saw succeed is
// in the file, and every commit under way when its writer was killed is
// there whole or not at all.
//
// In a build with the hypercrux_planted tag and
// HYPERCRUX_PLANT=procs/marker-before-batch, the toy log's writers put each
// marker down before its batch, and the test fails.
func TestTheToyLogPasses(t *testing.T) {
	rep, err := Run(toy, Options{Path: filepath.Join(t.TempDir(), "toy")})
	if err != nil {
		t.Fatal(err)
	}
	t.Log(rep)
}

// TestTheHarnessCatchesAMarkerBeforeItsBatch is the other half of T4's
// closing test: the toy log's writers put each commit's marker down before
// its batch, and pause between the two writes as they always do. A reader
// that finds the marker in the pause reads zeros where the batch goes, and
// a writer killed in the pause leaves the zeros marked for good. The
// harness catches it in every run, and stops at once.
func TestTheHarnessCatchesAMarkerBeforeItsBatch(t *testing.T) {
	runs := 5
	if testing.Short() {
		runs = 2
	}
	for i := range runs {
		began := time.Now()
		_, err := Run(toyMarkerFirst, Options{Path: filepath.Join(t.TempDir(), "toy")})
		mustFail(t, err, NotBegun, Disagree)
		t.Logf("run %d caught it in %v", i+1, time.Since(began).Round(time.Millisecond))
	}
}

// TestTheHarnessCatchesWritersWithoutTheLock: toy writers that never take
// the lock write over each other's commits, and the harness catches that
// too.
func TestTheHarnessCatchesWritersWithoutTheLock(t *testing.T) {
	_, err := Run(toyWithoutTheLock, Options{Path: filepath.Join(t.TempDir(), "toy"), Time: 300 * time.Millisecond})
	mustFail(t, err, Disagree, Missing, Lost, NotBegun, OutOfOrder, Twice)
}

// mustFail checks that err is a *Failure with one of the problems given,
// and returns it.
func mustFail(t *testing.T, err error, problems ...Problem) *Failure {
	t.Helper()
	var f *Failure
	if !errors.As(err, &f) {
		t.Fatalf("the harness found nothing wrong: %v", err)
	}
	if !slices.Contains(problems, f.Problem) {
		t.Fatalf("the harness found %v, where one of %v is wanted: %v", f.Problem, problems, err)
	}
	t.Log(err)
	return f
}

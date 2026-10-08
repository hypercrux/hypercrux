// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package crash is the crash-point driver. A test gives it a workload of
// commits, a way to open the database and read its state back, and, as the
// workload runs, a model of which commits succeeded. The driver runs the
// workload on beta/internal/fault's disk once to count its calls, then once
// for each point: the power cut in a write, a call failing, or a copy of
// the file taken from a call on, read as cp reads it. After each run it
// opens the database, or the copy, and checks that it holds the last
// commit that succeeded or the one under way. It reports the first point
// that fails, with its seed, so the failure replays.
//
// T3 built it, with a toy log in its tests. F3, F5 and F8 pass their
// closing tests through it on the real log. Like the rest of beta/, it
// builds only on Linux.
package crash

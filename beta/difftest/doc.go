// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package difftest runs random sequences of operations on two HyperCrux
// engines and compares every answer, allowing distances to differ by
// conformance.DistanceBound. A sequence that finds a difference is shrunk
// and saved as JSON, so it replays as a test from then on. Like the rest of
// beta/, it builds only on Linux.
package difftest

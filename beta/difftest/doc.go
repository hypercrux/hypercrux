// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package difftest runs random sequences of operations on two HyperCrux
// engines and compares every answer, allowing distances to differ by
// conformance.DistanceBound, and field names in case. A sequence that finds
// a difference is shrunk and saved as JSON, so it replays as a test from
// then on. Its tests run 0.x against itself, 0.x against the Beta without
// SQL until task G4, and 0.x against broken copies of itself, which it has
// to catch. Like the rest of beta/, it builds only on Linux.
package difftest

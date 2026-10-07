// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package sqlcorpus is a body of SQL with 0.x's answers: statements inside
// and outside the Beta's subset, a few thousand generated expressions and
// date cases. The corpus is in testdata as JSON lines. Its cases run on a
// small fixed database that Fixture builds through the conformance API, so
// any engine can build the same one and be compared with 0.x. Like the rest
// of beta/, it builds only on Linux.
package sqlcorpus

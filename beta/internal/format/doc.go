// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package format is for the Beta's file format, which beta/FORMAT.md
// describes byte by byte. It holds the change list, Change, which is what
// the format's batches carry, and the golden fixtures, in testdata, with
// the test that checks them. The codec that reads and writes the format
// joins them here (F1). Like the rest of beta/, it builds only on Linux.
package format

// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package format is the Beta's file format, which beta/FORMAT.md describes
// byte by byte. It holds the change list, Change, which is what the
// format's batches carry, and the codec: pure functions that encode and
// decode a header, a batch and a marker, with no file in sight, which the
// log uses to read and write the file. The golden fixtures are in
// testdata, with a test that checks them against FORMAT.md alone, apart
// from the codec. Like the rest of beta/, it builds only on Linux.
package format

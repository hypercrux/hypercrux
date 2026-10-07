// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package fsys is the Beta's file calls. Every call the engine makes on a
// file, or on the names in the database's folder, goes through its two
// interfaces, File and FS, so the crash tests can put a fault layer under
// them: T1's under the calls on data, T2's under the calls on names. The
// real calls go straight to Linux through Go's syscall package (F2). Like
// the rest of beta/, it builds only on Linux.
package fsys

// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package logfile is the Beta's database file: the log of batches that
// beta/FORMAT.md describes, and the write lock that keeps its writers
// apart. It creates a database, opens one and reads its log, handing each
// marked batch's changes to the in-memory copy, and appends a commit under
// the write lock: the batch, a sync, then the batch's marker. It reaches
// the file only through fsys, so the crash tests can put a fault layer
// under it.
//
// Tasks F2 to F9 build it. log.go says where each later task fits in. Like
// the rest of beta/, it builds only on Linux.
package logfile

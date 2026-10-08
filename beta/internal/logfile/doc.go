// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package logfile is the Beta's database file: the log of batches that
// beta/FORMAT.md describes, and the write lock that keeps its writers
// apart. It creates a database, opens one and reads its log, handing each
// marked batch's changes to the in-memory copy, and appends a commit under
// the write lock: the batch, a sync, then the batch's marker. Before it
// appends, and on opening when it gets the lock without waiting, it checks
// the end of the log: a batch whose writer died before marking it is
// written again, synced and marked, and what a crash left half written is
// cut off (check.go). First it looks past the end of the log for damage,
// such as a whole marker further on that shows a commit was made there,
// which it reports, cutting nothing; opening does that even when another
// holds the lock, and then reads the file again holding it before it
// reports damage (damage.go). A commit that fails is cut back out of the
// file before the lock goes, and when that fails too, the handle keeps the
// lock and refuses every write until it's closed (failed.go). It reaches the
// file only through fsys, so the crash tests can put a fault layer under it.
//
// Tasks F2 to F9 build it. log.go says where each later task fits in. Like
// the rest of beta/, it builds only on Linux.
package logfile

// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package procs is the many-process harness. A test gives it a workload:
// a writer's work, a reader's work, and a way to read the database once at
// the end. The harness runs writers and readers side by side on one
// database, each in a copy of the test binary that an environment variable
// sends to its role, and kills them with SIGKILL at random moments,
// starting new ones in their place. Each child reports to the parent
// through a pipe, a line at a time: a writer each commit it begins and how
// the commit ended, and a reader each commit it sees, in order.
//
// Once the time is up, the harness kills every writer, reads the file,
// waits for the readers to come to its end, and checks the run. Every
// reader saw every commit once and in order, and saw nothing a writer
// didn't begin. The readers agree on what each commit holds, and so does
// the file. A commit a writer saw succeed is in the file, and one under way
// when its writer was killed is there whole or not at all.
//
// A run can also take a backup of the database at random moments and move it
// into place a while later. Each restore begins an era of the log, and the
// checks are made era by era: each reader against the file of the era it
// reads, and a commit that succeeded against the file at the end, unless a
// restore lost it.
//
// T4 built it, with a toy log in its tests and a run on the real log. F6
// passes its closing test through it, with readers that follow the log, and
// F9 with compactions and backups moved into place, which F9 added. Like the
// rest of beta/, it builds only on Linux.
package procs

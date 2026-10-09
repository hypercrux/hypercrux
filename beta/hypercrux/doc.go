// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package hypercrux is the HyperCrux Beta's Go package, the engine BETA.md
// describes: a native hybrid SQL, key-value, graph and vector database in
// one file, written in Go without SQLite. It keeps 0.x's exported API apart
// from Adopt, ApplicationID and DriverName, which have no meaning without
// SQLite. It adds Compact, and the Beta's own error values, with Damage for
// the details of a damaged file.
//
// Until the Beta's release it sits here, at
// github.com/hypercrux/hypercrux/beta/hypercrux, beside 0.x's package at
// the module root, so that tests can run the two in one binary. The package
// name is hypercrux in both, so a program moves from one to the other by
// changing its import path. Like the rest of beta/, it builds only on
// Linux.
//
// What works so far, on DB and Tx alike, through the file: Open, Close,
// Update, Get, Put, Delete and TableOf (task G1), and Scan, Drop, Link,
// Unlink, Neighbours, Walk and Nearest without a filter (task G2). Export
// and Import on DB write and read 0.x's export format, the one EXPORT.md
// describes, byte for byte as 0.x does, so a database moves between the
// two either way (task G6). Check only counts until task G7. Each read
// through a DB follows what other processes commit, and reloads the file
// when a compaction or a backup moved into place has put another at the
// path (tasks F6 and F9); Compact, and the compaction a commit sets off,
// wait for task G5. The helpers that hold no state work as 0.x's do:
// ParseDirection, ParseVector, DecodeVector, and the methods of Vector,
// Direction and Link.
//
// Query, QueryRow, Exec and SQL() go through the package's database/sql
// driver (task G3), which parses each statement and takes its arguments,
// and hands it on inside a transaction's Update or through the database.
// SQL().Begin returns an error. Running a statement waits for task G4. The
// rest, running SQL and Nearest with a filter among them, returns an error
// that wraps errors.ErrUnsupported and names the task that makes it work.
package hypercrux

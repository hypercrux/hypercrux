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
// Unlink, Neighbours, Walk and Nearest (tasks G2 and G4). Export
// and Import on DB write and read 0.x's export format, the one EXPORT.md
// describes, byte for byte as 0.x does, so a database moves between the
// two either way (task G6). Check checks the copy in memory, reads the file
// again for damage, and gives the file's size and the copy's (task G7),
// and Export and Import refuse a database it finds problems in. Each read
// through a DB follows what other processes commit, and reloads the file
// when a compaction or a backup moved into place has put another at the
// path (tasks F6 and F9). Compact rewrites the file with only the live
// data (task G5), and a commit is followed by one once the commits since
// the last have made half the file dead, and a megabyte, or once 100,000
// commits follow it (task C1). Text and bytes hold at most 1,000,000,000
// bytes, SQLite's limit, as in 0.x, wherever they come in (task L1). The
// helpers that hold no state work as 0.x's do: ParseDirection, ParseVector,
// DecodeVector, and the methods of Vector, Direction and Link.
//
// Query, QueryRow, Exec and SQL() go through the package's database/sql
// driver (task G3), which parses each statement and takes its arguments,
// and hands it on inside a transaction's Update or through the database,
// where the engine runs it (task G4): the Beta's SQL, which beta/SQL.md
// sets out, with vectors as arguments, and errors that wrap ErrInvalid for
// a rule a write breaks. Nearest takes a filter in the same SQL. SQL().Begin
// returns an error.
//
// The hypercrux command in beta/cmd/hypercrux goes through this package.
package hypercrux

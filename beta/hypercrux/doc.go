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
// What works so far: Open, Close, Update, Get, Put, Delete and TableOf, on
// DB and Tx alike, through the file (task G1), and Check, which only counts
// until task G7. A DB reads other processes' commits when an Update of its
// own takes the write lock; following them between Updates is task F6's.
// The helpers that hold no state work as 0.x's do: ParseDirection,
// ParseVector, DecodeVector, and the methods of Vector, Direction and Link.
// The rest returns an error that wraps errors.ErrUnsupported and names the
// task that makes it work.
package hypercrux

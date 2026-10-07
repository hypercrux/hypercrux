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
// For now it's a skeleton (task P4). The helpers that hold no state work as
// 0.x's do: ParseDirection, ParseVector, DecodeVector, and the methods of
// Vector, Direction and Link. Open checks its path by 0.x's rules. The rest
// returns an error that wraps errors.ErrUnsupported and names the task that
// makes it work, starting with G1, which opens a database through the file.
package hypercrux

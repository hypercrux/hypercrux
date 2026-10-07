// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package hypercrux is the HyperCrux Beta's Go package, the engine BETA.md
// describes: a native hybrid SQL, key-value, graph and vector database in
// one file, written in Go without SQLite. It keeps 0.x's exported API apart
// from Adopt, ApplicationID and DriverName.
//
// Until the Beta's release it sits here, at
// github.com/hypercrux/hypercrux/beta/hypercrux, beside 0.x's package at
// the module root, so that tests can run the two in one binary. The package
// name is hypercrux in both, so a program moves from one to the other by
// changing its import path. Task P4 fills it with 0.x's API as stubs, and
// the tasks after it make them work. Like the rest of beta/, it builds only
// on Linux.
package hypercrux

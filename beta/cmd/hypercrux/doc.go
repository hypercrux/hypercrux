// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

// Command hypercrux is the HyperCrux Beta's command. It reads and writes the
// Beta's databases from the command line, through the Beta's Go package:
// records by key, the Beta's SQL, links and nearest-vector search, with
// import and export in 0.x's JSON lines, and compact. Other languages reach
// a database through it, reading and writing JSON, and each run opens the
// database afresh, reading the whole file.
//
// It has 0.x's verbs apart from adopt, with 0.x's output, messages and exit
// codes, so the command tests in beta/conformance/cmdtest run against both,
// leaving out the sections beta/SQL.md lists. It adds compact, and its check
// gives the file's size and the copy's on a line of its own after 0.x's.
// Exit codes: 0 when the verb worked, 1 when it failed or check found
// problems, and 2 for a mistake in how it was called, such as an unknown
// verb or option, or more than one SQL statement.
//
// Like the rest of beta/, it runs only on Linux. Elsewhere it builds all the
// same, so go build ./... works on macOS and Windows, and it says it runs on
// Linux only and exits with 1.
package main

import (
	"fmt"
	"os"
)

// command runs the command and gives its exit code. Only a Linux build has
// one, which main.go sets.
var command func() int

func main() {
	if command == nil {
		fmt.Fprintln(os.Stderr, "hypercrux: the Beta's command runs on Linux only, for now; HyperCrux 0.x's runs on macOS and Windows")
		os.Exit(1)
	}
	os.Exit(command())
}

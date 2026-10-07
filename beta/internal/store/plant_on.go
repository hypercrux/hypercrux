// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux && hypercrux_planted

package store

import "os"

// A build with the hypercrux_planted tag can switch on one of this
// package's planted bugs, named in HYPERCRUX_PLANT. scripts/planted.sh runs
// the tests with each bug listed in beta/plants.txt, and every one must
// make them fail. A name this package doesn't know switches nothing on.
//
//   - store/nulls-kept: a put that sets a field to null keeps the null in
//     the record.
//   - store/drop-keeps-records: Drop leaves the table's records behind.
//   - store/spelt-as-given: a Put change spells the fields as the put gave
//     them, where the table spells them otherwise.
//   - store/size-unchecked: a put takes a vector of another size than the
//     table's.
//   - store/undo-oldest-first: a rollback walks the undo list from its
//     oldest entry instead of its newest.
//   - store/changes-kept: RollbackTo takes back the changes since its mark,
//     and leaves them in the change list.
//   - store/readers-not-held: a transaction's first change doesn't take the
//     copy's lock, so readers see changes that aren't committed.
//   - store/readers-in-early: Commit lets readers in while it hands the
//     change list on, before the changes are in the file.
//   - store/inside-unchecked: Read skips the goroutine check, so a read
//     through the database inside Update after its first change waits for
//     itself.
var plant = func() string {
	switch p := os.Getenv("HYPERCRUX_PLANT"); p {
	case "store/nulls-kept", "store/drop-keeps-records", "store/spelt-as-given", "store/size-unchecked",
		"store/undo-oldest-first", "store/changes-kept", "store/readers-not-held", "store/readers-in-early",
		"store/inside-unchecked":
		return p
	}
	return ""
}()

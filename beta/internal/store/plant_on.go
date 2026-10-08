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
//   - store/batch-half-applied: ApplyBatch keeps the changes before the one
//     that fails.
//   - store/unused-fields-taken: Apply takes a change that sets a field its
//     Op doesn't use, which the codec can't write.
//   - store/applied-slices-shared: a transaction's Apply puts the caller's
//     Names and Fields in the change list, so a caller that uses them again
//     changes the list.
//   - store/snapshot-in-map-order: the snapshot gives each table's records
//     in the hash table's order instead of byte order of key.
//   - store/snapshot-fields-unsorted: a Put in the snapshot gives the
//     record's fields in the table's order instead of byte order of name.
//   - store/snapshot-no-vectors: a Put in the snapshot leaves the vector
//     out.
//   - store/snapshot-size-left-out: a CreateTable in the snapshot gives the
//     vector size 0.
//   - store/order-not-undone: a rollback leaves each table's keys as the
//     transaction left them.
//   - store/delete-keeps-key: a delete leaves the record's key among its
//     table's keys.
//   - store/join-out-of-order: two blocks of a table's keys that join put
//     the later block's keys first.
//   - store/after-included: Scan gives the record whose key is after, as
//     well as the ones that come after it.
//   - store/cursor-keeps-its-place: a cursor carries on from its old place
//     in the blocks after a change, without finding its place again.
//   - store/prefix-table-unchecked: Scan takes a prefix whose table name
//     breaks the rules, such as Docs:, and gives no records.
//   - store/link-added-twice: Link adds a link that's there already, with a
//     second pair of halves and a second Link change.
//   - store/unlink-one-end: Unlink takes a link out of the list of the
//     record it's from, and leaves its half in the list of the record it's
//     to.
//   - store/unlink-types-reversed: Unlink with no type gives its Unlink
//     changes in reverse byte order of type.
//   - store/links-in-kept: a delete or a drop leaves the links that point
//     into its records in the lists of the records they're from.
//   - store/link-undone-at-one-end: undoing a link takes it out of the list
//     of the record it's from only.
//   - store/both-not-merged: Neighbours both ways gives the links out, then
//     the links in, unmerged, with a link from the record to itself twice.
//   - store/walk-unsorted: a walk gives each round's records in the order it
//     found them, instead of by key.
//   - store/walk-start-included: a walk gives the record it started from
//     when a path leads back to it.
//   - store/snapshot-links-by-table: the snapshot gives the links in byte
//     order of their tables' names, users before users2, instead of the
//     order of their keys.
//   - store/freed-slot-taken-at-once: a slot that a delete or a vector set
//     to null frees inside a transaction goes on the free list at once,
//     where a vector the transaction puts can take it before a rollback
//     gives it back to its record.
//   - store/overwrite-not-kept: a vector written over in its slot keeps no
//     copy of the old values in its undo entry, so a rollback leaves the
//     new ones.
//   - store/norm-not-undone: a rollback of a vector written over puts back
//     its values and leaves the new vector's norm.
//   - store/deleted-vector-found: a delete leaves its record's slot in use,
//     so a search still finds the record.
//   - store/ties-by-slot: a search breaks a tie in distance by slot instead
//     of by key.
//   - store/filter-after-distance: a search works out each distance first
//     and calls the Filter only for the records near enough to join the
//     closest k, so it's called fewer times than there are vectors, and
//     after their dot products.
//   - store/top-k-first-seen: a search keeps the first k vectors it meets
//     instead of the closest k.
//   - store/nearest-table-first: Nearest looks for the table before it
//     checks k, so a bad k on a table that isn't there gives not found.
var plant = func() string {
	switch p := os.Getenv("HYPERCRUX_PLANT"); p {
	case "store/nulls-kept", "store/drop-keeps-records", "store/spelt-as-given", "store/size-unchecked",
		"store/undo-oldest-first", "store/changes-kept", "store/readers-not-held", "store/readers-in-early",
		"store/inside-unchecked", "store/batch-half-applied", "store/unused-fields-taken",
		"store/applied-slices-shared", "store/snapshot-in-map-order", "store/snapshot-fields-unsorted",
		"store/snapshot-no-vectors", "store/snapshot-size-left-out", "store/order-not-undone",
		"store/delete-keeps-key", "store/join-out-of-order", "store/after-included",
		"store/cursor-keeps-its-place", "store/prefix-table-unchecked", "store/link-added-twice",
		"store/unlink-one-end", "store/unlink-types-reversed", "store/links-in-kept", "store/link-undone-at-one-end",
		"store/both-not-merged", "store/walk-unsorted", "store/walk-start-included", "store/snapshot-links-by-table",
		"store/freed-slot-taken-at-once", "store/overwrite-not-kept", "store/norm-not-undone",
		"store/deleted-vector-found", "store/ties-by-slot", "store/filter-after-distance", "store/top-k-first-seen",
		"store/nearest-table-first":
		return p
	}
	return ""
}()

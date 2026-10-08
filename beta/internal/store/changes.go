// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store

import (
	"fmt"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
)

// Change lists (S3). A transaction's writes append the changes they amount
// to as they run, and Commit hands the list on in that order, for the log
// to write as one batch. What the writes append keeps FORMAT.md's rules for
// what a writer emits, which format.AppendBatch checks, so a commit never
// fails there:
//
//   - a put into a table that doesn't exist yet comes after a CreateTable
//     with no fields and size 0;
//   - a put's fields come in byte order of name, spelt as the table spells
//     them, with values the format can hold;
//   - a change sets only the fields of format.Change that its Op uses;
//   - a write that would do nothing fails instead, apart from a put, and a
//     transaction that changes nothing hands on no list, so the log writes
//     no batch.
//
// A batch the log has read goes into the store whole or not at all, by one
// of two calls. ApplyBatch is for a store that others read: the batch goes
// in through a transaction of its own, so readers see all of it or none,
// and a change that fails rolls it back. LoadBatch is for a store that
// nobody reads yet, as on opening: the changes go straight into the store,
// and an error leaves the store to be thrown away. Either way, a change
// that breaks the rules for the state it applies to is damage, since a
// batch that counts holds what its writer wrote.

// ApplyBatch applies the batch numbered seq, which the log has read, all of
// it or none. The changes go in through a transaction of their own, which
// waits for one that's open and takes the copy's lock at the first change,
// so readers see none of the batch until all of it is in. It's for a store
// that others read: the writer catching up under the write lock, or a
// process following the log.
//
// A change that breaks the rules for the state the changes before it left
// gives a *errs.Damage naming the batch by seq, with the change's number
// from 1 and what's wrong, and leaves the store as it was. A batch with no
// changes is damage too. The Damage has no path, and an Offset of 0: the
// log knows the file and where the batch starts. On the goroutine running
// the store's open transaction, ApplyBatch returns errs.ErrInsideUpdate at
// once, as Begin does, since it would wait for that transaction.
func (s *Store) ApplyBatch(seq uint64, changes []format.Change) error {
	if len(changes) == 0 {
		return noChanges(seq)
	}
	tx, err := s.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() // takes back every change after a failure or a panic
	for i := range changes {
		if err := s.apply(changes[i]); err != nil {
			if plant == "store/batch-half-applied" {
				tx.Commit(nil)
			}
			return damage(seq, i, err)
		}
	}
	// The batch is in the file already, so nothing is handed on, and the
	// transaction keeps no change list: its changes go straight to apply.
	return tx.Commit(nil)
}

// LoadBatch applies the batch numbered seq, which the log has read, to a
// store that nobody else reads yet, as on opening a database. The changes
// go straight into the store, as the store's own Apply makes them. That
// saves what a transaction costs on every batch: Begin's call to
// runtime.Stack, the locks, and the undo list.
//
// A change that breaks the rules gives a *errs.Damage, as in ApplyBatch,
// and leaves the store holding the changes before it. So the caller throws
// the store away, as an open that fails does, and the batch counts as not
// applied at all. LoadBatch panics while a transaction is open, since a
// store with one is a store that others read.
func (s *Store) LoadBatch(seq uint64, changes []format.Change) error {
	if s.tx != nil {
		panic("store: LoadBatch while a transaction is open; a store that others read takes a batch through ApplyBatch")
	}
	if len(changes) == 0 {
		return noChanges(seq)
	}
	for i := range changes {
		if err := s.apply(changes[i]); err != nil {
			return damage(seq, i, err)
		}
	}
	return nil
}

// damage is the error for change i, from 0, of the batch numbered seq,
// which broke the rules for the state it applied to: err says how. It
// wraps errs.ErrDamaged alone, whatever err wraps, so its kind is "error".
func damage(seq uint64, i int, err error) error {
	return &errs.Damage{Batch: seq, Reason: fmt.Sprintf("change %d: %v", i+1, err)}
}

// noChanges is the error for a batch with no changes, in the codec's words.
func noChanges(seq uint64) error {
	return &errs.Damage{Batch: seq, Reason: "a batch with no changes"}
}

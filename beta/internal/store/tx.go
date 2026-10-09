// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store

import (
	"fmt"
	"iter"
	"slices"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/rules"
)

// Transactions (S2). An Update in the public package runs like this (G1):
//
//	if err := s.Outside(); err != nil { // a write through db inside Update
//		return err
//	}
//	// take the write lock and catch up (F2, F6)
//	tx, err := s.Begin()
//	if err != nil {
//		return err
//	}
//	defer tx.Rollback() // does nothing once Commit has ended tx
//	if err := fn(tx); err != nil { // through the public package's Tx
//		return err
//	}
//	return tx.Commit(log.Append) // the batch, its sync and its marker
//
// SQL's writes (Q6) mark the start of each statement with Mark, and a
// statement that fails takes its own changes back with RollbackTo, while
// the transaction carries on. A batch applied from the log (S3, F6) is a
// transaction of its changes, committed with no write, or rolled back
// whole at the first change that fails: Store.ApplyBatch, in changes.go.

// Tx is a transaction on the store: the writes of one Update, or one batch
// applied from the log. Its changes go straight into the copy. An undo list
// records how to reverse each one, and a change list records them for the
// log. Commit keeps them and hands the change list on, and Rollback puts the
// copy back as it was at Begin. A write that fails undoes its own changes
// and leaves the transaction open.
//
// A Tx is a Reader too, and sees its own changes. One goroutine uses it at
// a time. Until its first change it holds up no reader, and reads beside
// them. Its first change takes the copy's lock alone, and readers wait
// from then until it ends, while the Tx itself reads and writes without
// waiting. What a read through it hands out is good until its next change.
//
// Once Commit or Rollback has ended it, its methods fail with an error that
// wraps errs.ErrClosed, Table finds no table, ranging over Snapshot panics,
// and Rollback does nothing.
type Tx struct {
	s       *Store
	undo    []undo          // how to reverse each change so far, newest last
	changes []format.Change // the change list
	locked  bool            // the transaction holds s.mu, from its first change
	done    bool            // Commit or Rollback has ended it
}

var _ Reader = (*Tx)(nil)

// Begin starts a transaction. It waits while another is open, so one runs
// at a time. The transaction notes the goroutine that began it, by the
// number runtime.Stack shows, for the check behind errs.ErrInsideUpdate:
// on the goroutine running the open transaction, Begin returns that error
// at once instead of waiting for itself.
//
// Begin takes no lock that readers wait for. The transaction's first change
// does.
func (s *Store) Begin() (*Tx, error) {
	me := goroutine()
	if s.owner.Load() == me {
		return nil, errs.ErrInsideUpdate
	}
	s.writer.Lock()
	s.owner.Store(me)
	tx := &Tx{s: s, undo: s.spare}
	s.spare = nil
	s.tx = tx
	return tx, nil
}

// maxSpare is the most entries an undo list keeps for the next transaction,
// about a megabyte of them. A thousand puts of new records make 3,000, and an
// undo list grown from nothing for each transaction took about a sixteenth of
// a batched put's time (F7).
const maxSpare = 8192

// Read calls fn with the store as a Reader, under the copy's lock held
// shared, and returns fn's error. fn sees one point in the log: the copy as
// the last commit left it. Reads share the lock. A read waits while a
// transaction holds it, from that transaction's first change until its
// end, and a transaction's first change waits for the reads under way.
// What fn is handed is good until fn returns. fn mustn't make a change
// through a transaction, since that would wait for fn.
//
// On the goroutine running a transaction that has made its first change,
// Read returns errs.ErrInsideUpdate at once, since it would wait for that
// same transaction. That's the check for a read through the database
// inside Update; the transaction reads through the Tx. Before the first
// change such a read goes ahead, and sees nothing of the transaction.
func (s *Store) Read(fn func(r Reader) error) error {
	if s.held.Load() && plant != "store/inside-unchecked" && s.inside() {
		return errs.ErrInsideUpdate
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return fn(s)
}

// Outside returns errs.ErrInsideUpdate when the calling goroutine is running
// the store's open transaction, and nil otherwise. A write through the
// database there would wait for that transaction, since every such write is
// an Update of its own. So the public package's Update calls Outside first,
// before it waits for the write lock, and anything else that takes the
// write lock, such as Compact, does the same. Reads need no call, since Read
// checks by itself, and so does Begin.
//
// While no transaction is open, Outside costs one atomic load. While one is,
// it costs a call to runtime.Stack, and the call it guards would wait for
// the transaction anyway.
func (s *Store) Outside() error {
	if s.owner.Load() != 0 && s.inside() {
		return errs.ErrInsideUpdate
	}
	return nil
}

// inside reports whether the calling goroutine began the open transaction.
// Only that goroutine can make owner its own number, so the answer can't
// go stale while it's asking.
func (s *Store) inside() bool {
	o := s.owner.Load()
	return o != 0 && o == goroutine()
}

// changing comes just before each change to the store, with the undo entry
// that reverses it. Inside a transaction, the first change takes the copy's
// lock, so readers wait from then on, and the entry joins the undo list.
// Outside one it does nothing, since a direct write has the store to
// itself.
func (s *Store) changing(u undo) {
	tx := s.tx
	if tx == nil {
		return
	}
	if !tx.locked && plant != "store/readers-not-held" {
		s.mu.Lock()
		tx.locked = true
		s.held.Store(true)
	}
	tx.undo = append(tx.undo, u)
}

// undo is one entry in a transaction's undo list: how one part of the store
// was just before a change. Putting that back reverses the change. An entry
// records the whole earlier state of its part, so putting it back works
// even when a panic cut its change short. A rollback walks the list newest
// first, so each entry finds the store as its own change left it.
//
// S4 added the kinds for each table's keys, S5 the kinds for the links, and
// V1 the kinds for the vector arrays.
type undo struct {
	op     undoOp
	name   string       // undoNoTable: the table's name; undoNoRecord: the key
	table  *table       // undoHadTable, undoFields, undoSize, undoNoKey, undoHadKey, and the table whose array the slots' kinds change
	record *record      // undoHadRecord, undoRecord, undoNoKey, undoHadKey, undoFreedSlot, and the record whose list the links' kinds change
	n      int          // undoFields: how many fields the table had; undoSize: its size; undoRecord: the record's slot; the slots' kinds: the slot
	fields []FieldValue // undoRecord: the record's fields
	vec    []float32    // undoVector: the slot's values
	norm   float64      // undoVector: the slot's norm
	half   half         // undoNoLink, undoHadLink: the link's type and the record it's to; undoHadOut, undoHadIn: the half
}

type undoOp uint8

const (
	undoNoTable   undoOp = iota + 1 // no table was called name
	undoHadTable                    // table was in the store, whole
	undoFields                      // table had its first n fields only
	undoSize                        // table had the vector size n, and an empty array when n is 0
	undoNoRecord                    // no record had the key name
	undoHadRecord                   // record was in the store, whole
	undoRecord                      // record held fields, and its vector in slot n, or none when n is -1
	undoNoKey                       // table's keys didn't hold record's key
	undoHadKey                      // table's keys held record's key, with record
	undoNoLink                      // there was no link of half's type from record to half's record
	undoHadLink                     // there was that link, with both its halves
	undoHadOut                      // record's links out held half
	undoHadIn                       // record's links in held half
	undoNewSlot                     // table's array had n slots
	undoTookSlot                    // slot n of table's array was free, last on its free list
	undoFreedSlot                   // slot n of table's array held record's vector, as it still does
	undoVector                      // slot n of table's array held vec, with the norm norm
)

// back puts back the state one entry records. Each table's keys are put
// back by key, and each record's links by type and key, so a block that
// split or joined since needn't be put back as it was: only which keys a
// table holds counts, and which halves a list holds.
func (s *Store) back(u undo) {
	switch u.op {
	case undoNoLink:
		a, b := u.record, u.half.other
		a.out.remove(linkAt{u.half.typ.Value(), b.key})
		if plant != "store/link-undone-at-one-end" {
			b.in.remove(linkAt{u.half.typ.Value(), a.key})
		}
	case undoHadLink:
		a, b := u.record, u.half.other
		a.out.insert(u.half)
		b.in.insert(half{u.half.typ, a})
	case undoHadOut:
		u.record.out.insert(u.half)
	case undoHadIn:
		u.record.in.insert(u.half)
	case undoNoTable:
		delete(s.tables, u.name)
		s.epoch++
	case undoHadTable:
		s.tables[u.table.name] = u.table
		s.epoch++
	case undoNoKey:
		if plant != "store/order-not-undone" {
			u.table.keys.remove(u.record.key)
		}
		s.epoch++
	case undoHadKey:
		if plant != "store/order-not-undone" {
			u.table.keys.insert(u.record)
		}
		s.epoch++
	case undoFields:
		t := u.table
		for _, name := range t.fields[u.n:] {
			delete(t.index, rules.Fold(name))
		}
		if t.vec >= u.n {
			t.vec = -1
		}
		// A list cut back gets no room past its end, so a field added
		// later never lands in an array a read handed out.
		t.fields = t.fields[:u.n:u.n]
		if u.n == 0 {
			t.fields = nil // as a new table has it
		}
	case undoSize:
		u.table.size = u.n
		if u.n == 0 {
			// The table's first vector is taken back, and so is every slot
			// after it, since they're newer: the array goes back to empty,
			// and the next first vector sets its shape again.
			u.table.vecs = vectors{}
		}
	case undoNoRecord:
		delete(s.records, u.name)
	case undoHadRecord:
		s.records[u.record.key] = u.record
	case undoRecord:
		u.record.fields, u.record.slot = u.fields, u.n
	case undoNewSlot:
		a := &u.table.vecs
		clear(a.owners[u.n:]) // so the array keeps no record alive
		a.owners, a.norms = a.owners[:u.n], a.norms[:u.n]
	case undoTookSlot:
		a := &u.table.vecs
		a.owners[u.n] = nil
		a.free = append(a.free, u.n)
	case undoFreedSlot:
		u.table.vecs.owners[u.n] = u.record
	case undoVector:
		copy(u.table.vector(u.n), u.vec)
		if plant != "store/norm-not-undone" {
			u.table.vecs.norms[u.n] = u.norm
		}
	default:
		panic(fmt.Sprintf("store: an undo entry of kind %d", u.op))
	}
}

// undoTo walks the undo list back to its first n entries, newest first.
func (tx *Tx) undoTo(n int) {
	if plant == "store/undo-oldest-first" {
		for _, u := range tx.undo[n:] {
			tx.s.back(u)
		}
	} else {
		for i := len(tx.undo) - 1; i >= n; i-- {
			tx.s.back(tx.undo[i])
		}
	}
	clear(tx.undo[n:]) // so the old fields and records can be collected
	tx.undo = tx.undo[:n]
}

// Mark is a point in a transaction, for RollbackTo.
type Mark struct{ undo, changes int }

// Mark returns the transaction's present point, for RollbackTo.
func (tx *Tx) Mark() Mark { return Mark{len(tx.undo), len(tx.changes)} }

// RollbackTo takes back every change made since m, newest first, and cuts
// the change list back to where it was at m. The transaction carries on,
// and keeps the copy's lock if it has it. A mark that's past the
// transaction's present point, after an earlier RollbackTo, panics. On a
// transaction that has ended, RollbackTo does nothing.
func (tx *Tx) RollbackTo(m Mark) {
	if tx.done {
		return
	}
	if m.undo > len(tx.undo) || m.changes > len(tx.changes) {
		panic(fmt.Sprintf("store: RollbackTo a mark at %d changes, past the transaction's %d", m.changes, len(tx.changes)))
	}
	tx.undoTo(m.undo)
	if plant != "store/changes-kept" {
		clear(tx.changes[m.changes:])
		tx.changes = tx.changes[:m.changes]
	}
}

// Commit ends the transaction and keeps its changes. When it made changes
// and write isn't nil, Commit first hands write the change list, with the
// copy still locked, so other goroutines read the changes only once write
// has put them in the file. In an Update, write is the log's append, sync
// and marker (G1), and for a batch applied from the log it's nil. If write
// returns an error or panics, Commit puts the copy back as Rollback does,
// then returns the error or panics again. The change list is write's to
// keep, and the transaction doesn't touch it again.
//
// On a transaction that has ended, Commit fails with an error that wraps
// errs.ErrClosed.
func (tx *Tx) Commit(write func(changes []format.Change) error) error {
	if err := tx.open(); err != nil {
		return err
	}
	if write != nil && len(tx.changes) > 0 {
		if err := tx.handOn(write); err != nil {
			return err
		}
	}
	tx.release()
	tx.end()
	return nil
}

// release puts each slot the transaction freed on its table's free list,
// once nothing can take the transaction back (vectors.go). Until then a
// freed slot is on no list, so no vector takes it while a rollback might
// give it back to its record. A slot of a table that the transaction then
// dropped goes on that table's list, which goes with the table.
func (tx *Tx) release() {
	if plant == "store/freed-slot-taken-at-once" {
		return // they're on the lists already
	}
	for _, u := range tx.undo {
		if u.op == undoFreedSlot {
			u.table.vecs.free = append(u.table.vecs.free, u.n)
		}
	}
}

// handOn hands write the change list, and rolls the transaction back if
// write fails or panics.
func (tx *Tx) handOn(write func([]format.Change) error) (err error) {
	written := false
	defer func() {
		if !written {
			tx.Rollback()
		}
	}()
	if plant == "store/readers-in-early" && tx.locked {
		tx.s.mu.Unlock()
		defer tx.s.mu.Lock() // before the rollback above, which runs last
	}
	err = write(tx.changes)
	written = err == nil
	return err
}

// Rollback ends the transaction and takes back every change it made, newest
// first, so the copy is as it was at Begin. On a transaction that has ended
// it does nothing, so a caller can defer it straight after Begin.
func (tx *Tx) Rollback() {
	if tx.done {
		return
	}
	tx.undoTo(0)
	tx.end()
}

// end releases the copy's lock, if the transaction took it, and then the
// store for the next transaction. The undo list's room goes to the next
// transaction, unless it's grown large, with every entry cleared first, so it
// keeps nothing alive.
func (tx *Tx) end() {
	s := tx.s
	tx.done = true
	if cap(tx.undo) <= maxSpare {
		clear(tx.undo)
		s.spare = tx.undo[:0]
	}
	tx.undo, tx.changes = nil, nil
	s.tx = nil
	if tx.locked {
		tx.locked = false
		s.held.Store(false)
		s.mu.Unlock()
	}
	// The owner goes before the writer's lock does, so a transaction that
	// begins next never has its number cleared.
	s.owner.Store(0)
	s.writer.Unlock()
}

func (tx *Tx) open() error {
	if tx.done {
		return fmt.Errorf("%w: the transaction has ended", errs.ErrClosed)
	}
	return nil
}

// Put is Store.Put inside the transaction. Its changes join the
// transaction's change list. On an error, the transaction is as it was
// before the call, and carries on.
func (tx *Tx) Put(key string, fields []format.Field) error {
	return tx.write(func(dst []format.Change) ([]format.Change, error) { return tx.s.putFields(dst, key, fields) })
}

// Delete is Store.Delete inside the transaction.
func (tx *Tx) Delete(key string) error {
	return tx.write(func(dst []format.Change) ([]format.Change, error) { return tx.s.deleteKey(dst, key) })
}

// Drop is Store.Drop inside the transaction.
func (tx *Tx) Drop(name string) error {
	return tx.write(func(dst []format.Change) ([]format.Change, error) { return tx.s.dropTable(dst, name) })
}

// Link is Store.Link inside the transaction.
func (tx *Tx) Link(from, typ, to string) error {
	return tx.write(func(dst []format.Change) ([]format.Change, error) { return tx.s.linkKeys(dst, from, typ, to) })
}

// Unlink is Store.Unlink inside the transaction.
func (tx *Tx) Unlink(from, typ, to string) error {
	return tx.write(func(dst []format.Change) ([]format.Change, error) { return tx.s.unlinkKeys(dst, from, typ, to) })
}

// Apply is Store.Apply inside the transaction: one change of a change list,
// such as an import's CreateTable with a table's whole shape, which joins
// the transaction's change list too. The list keeps copies of the change's
// Names and Fields, so the caller can use its slices again, as a snapshot
// does with a Put's Fields. A batch from the log goes in whole through
// Store.ApplyBatch instead.
func (tx *Tx) Apply(c format.Change) error {
	return tx.write(func(dst []format.Change) ([]format.Change, error) {
		if err := tx.s.apply(c); err != nil {
			return dst, err
		}
		if plant != "store/applied-slices-shared" {
			c.Names, c.Fields = clip(slices.Clone(c.Names)), clip(slices.Clone(c.Fields))
		}
		return append(dst, c), nil
	})
}

// write runs one write of the store's, which appends to the change list.
// If it fails, everything it changed is taken back, so a write that fails
// part of the way through leaves the transaction as it was. The store's
// writes check everything first, so for now they change nothing when they
// fail, but the transaction doesn't count on it.
func (tx *Tx) write(w func(dst []format.Change) ([]format.Change, error)) error {
	if err := tx.open(); err != nil {
		return err
	}
	m := tx.Mark()
	changes, err := w(tx.changes)
	if err != nil {
		tx.RollbackTo(m)
		return err
	}
	tx.changes = changes
	return nil
}

// The read side: the store's, with the transaction's changes in it.

// Table is Store.Table inside the transaction. On a transaction that has
// ended it finds no table.
func (tx *Tx) Table(name string) (Table, bool) {
	if tx.done {
		return Table{Vec: -1}, false
	}
	return tx.s.Table(name)
}

// Get is Store.Get inside the transaction.
func (tx *Tx) Get(key string) (Record, error) {
	if err := tx.open(); err != nil {
		return Record{}, err
	}
	return tx.s.Get(key)
}

// Scan is Store.Scan inside the transaction, with the transaction's changes
// in it. The Cursor carries on through the transaction's later changes,
// statements taken back included: each record it gives is the first after
// the last one it gave, among the records the table holds at that moment.
// Once the transaction has ended, it gives no more.
func (tx *Tx) Scan(prefix, after string) (Cursor, error) {
	if err := tx.open(); err != nil {
		return nil, err
	}
	return tx.s.scan(tx, prefix, after)
}

// Neighbours is Store.Neighbours inside the transaction.
func (tx *Tx) Neighbours(key string, dir Direction, typ string) ([]Link, error) {
	if err := tx.open(); err != nil {
		return nil, err
	}
	return tx.s.Neighbours(key, dir, typ)
}

// Walk is Store.Walk inside the transaction.
func (tx *Tx) Walk(key string, dir Direction, typ string, depth int) ([]Step, error) {
	if err := tx.open(); err != nil {
		return nil, err
	}
	return tx.s.Walk(key, dir, typ, depth)
}

// Nearest is Store.Nearest inside the transaction.
func (tx *Tx) Nearest(table string, q []float32, k int, keep Filter) ([]Hit, error) {
	if err := tx.open(); err != nil {
		return nil, err
	}
	return tx.s.Nearest(table, q, k, keep)
}

// Snapshot is Store.Snapshot inside the transaction, with its changes in
// it. Compaction takes its snapshot through Read after the commit instead
// (P3's note for F8).
func (tx *Tx) Snapshot() iter.Seq[format.Change] {
	if tx.done {
		return func(func(format.Change) bool) { panic(tx.open()) }
	}
	return tx.s.Snapshot()
}

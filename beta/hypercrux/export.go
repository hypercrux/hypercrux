// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux

import (
	"cmp"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/rules"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
	"github.com/hypercrux/hypercrux/internal/export"
)

// Export and import (task G6) go through internal/export, the package that
// reads and writes the format for 0.x's Export and Import too. So the two
// write the same bytes for the same data, and read the same exports, with
// the same leniencies. EXPORT.md, at the root of the repository, describes
// the format.

// Export writes the whole database to w as JSON lines, in the format
// EXPORT.md describes, which 0.x's Export writes too. Import reads it back,
// here or in 0.x, so a database moves between the two either way, and the
// same data gives the same bytes from both.
//
// Export reads a single point in the log, in one read of the copy in
// memory. Other processes carry on writing while it runs. In this process,
// an Update waits from its first change until the export ends, as it waits
// for any read, and inside an Update, Export works only until the first
// change, as every read through the database does. Once task G7 gives
// Check its checks, Export refuses a database that Check finds problems in,
// as 0.x's does. An export that stops on an error, such as one from w, has
// no last line, and Import refuses an export without it.
func (db *DB) Export(w io.Writer) error {
	if plant == "hypercrux/export-unlocked" {
		s, err := db.current()
		if err != nil {
			return err
		}
		return exportAll(s, export.NewWriter(w))
	}
	return db.read(func(r store.Reader) error { return exportAll(r, export.NewWriter(w)) })
}

// exportAll writes what r holds to w, from r's snapshot. The snapshot comes
// in the export's order, the tables in byte order of name, each table's
// records in byte order of key, and the links in byte order of from, type
// and to, apart from two things. It gives each table's records straight
// after the table, where an export lists every table first, so exportAll
// ranges over it twice: once for the tables, and once for the records and
// the links. And a put in it gives its fields in byte order of name, where
// an export gives them in the table's order, so exportAll puts them back in
// that order. The two passes read the copy at the one point r holds it at.
//
// Every value in the copy is one Put took, or one a batch brought that the
// store checked by the same rules, so the writer takes all of them. The
// writer checks the order of what it's given, so if the store ever gave
// one out of order, the export would stop there with an error.
func exportAll(r store.Reader, w *export.Writer) error {
	for c := range r.Snapshot() {
		if c.Op != format.CreateTable {
			continue
		}
		t := export.Table{Name: c.Table, Dims: c.Size, Fields: c.Names}
		if plant == "hypercrux/export-fields-sorted" {
			t.Fields = slices.Sorted(slices.Values(c.Names))
		}
		if err := w.Table(t); err != nil {
			return err
		}
	}
	var (
		place  map[string]int // each field's place in the table whose puts come now
		fields []placed       // the put's fields, put in the table's order
		rec    export.Record
		vec    []float32 // the put's vector, which the writer writes at once
	)
	for c := range r.Snapshot() {
		switch c.Op {
		case format.CreateTable:
			names := c.Names
			if plant == "hypercrux/export-fields-sorted" {
				names = slices.Sorted(slices.Values(c.Names))
			}
			place = make(map[string]int, len(names))
			for i, name := range names {
				place[name] = i
			}
		case format.Put:
			fields = fields[:0]
			for _, f := range c.Fields {
				var v any
				if f.Value.Kind() == value.KindVector {
					vec = f.Value.AppendVector(vec[:0])
					v = vec
				} else {
					v = f.Value.Go()
				}
				fields = append(fields, placed{place[f.Name], export.Field{Name: f.Name, Value: v}})
			}
			slices.SortFunc(fields, func(a, b placed) int { return cmp.Compare(a.at, b.at) })
			rec.Key, rec.Fields = c.Key, rec.Fields[:0]
			for _, f := range fields {
				rec.Fields = append(rec.Fields, f.Field)
			}
			if err := w.Record(rec); err != nil {
				return err
			}
		case format.Link:
			if err := w.Link(export.Link{From: c.Key, Type: c.Type, To: c.To}); err != nil {
				return err
			}
		}
	}
	return w.Close()
}

// placed is a field of a record with its place in its table's fields.
type placed struct {
	at int
	export.Field
}

// Import reads an export, as Export writes it, into this database, which
// must hold no tables yet: one that's new, or one whose tables have all been
// dropped. It all goes in one transaction, which commits as one batch: if
// the export is cut short, or anything in it breaks the rules Put and Link
// follow, nothing is kept and nothing reaches the file. Tables come back
// with their fields in the same order and the same vector sizes, and every
// value goes through Put's conversions, so exporting the result gives the
// same bytes.
//
// Import reads r inside its Update, holding the write lock, as 0.x's does
// inside its transaction. From the first table it reads until it commits,
// the rest of this process waits to read, as it does for any Update, while
// other processes read as usual. Inside an Update, Import fails at once, as
// every write through the database does.
//
// It refuses what 0.x's refuses, with 0.x's kinds, and in 0.x's words
// wherever the Beta's rules are 0.x's. Each refusal wraps ErrInvalid:
// input that isn't a valid export reads "not a valid HyperCrux export", a
// database that holds tables, even empty ones, reads "already holds record
// tables", and anything Put or Link would refuse, or a record or a link
// that's there twice, names the line of the export. Two refusals are the
// Beta's own. A table can't have a vector size without a vector field in
// the Beta, as it can in 0.x. And a table of more than 1,999 fields, which
// 0.x's SQLite refuses with a plain error of its own, is invalid here, as
// a Put that made one would be.
func (db *DB) Import(r io.Reader) error {
	s, err := db.current()
	if err != nil {
		return err
	}
	if err := s.Outside(); err != nil {
		return err
	}
	rd, err := export.NewReader(r)
	if err != nil {
		return importError(err)
	}
	var kept error // the error of an import that commits all the same, a planted bug's
	err = db.Update(func(tx *Tx) error {
		err := importAll(tx.stx, rd)
		if err != nil && plant == "hypercrux/import-kept-in-part" {
			kept = err
			return nil
		}
		return err
	})
	if err == nil {
		err = kept
	}
	return err
}

// importAll reads the export's tables, records and links from rd into tx,
// which must hold no tables. It stops at the first that breaks a rule, and
// the Update then rolls tx back.
func importAll(tx *store.Tx, rd *export.Reader) error {
	if holdsTables(tx) && plant != "hypercrux/import-over-tables" {
		return fmt.Errorf("%w: the file already holds record tables; import into a new file", ErrInvalid)
	}
	// 0.x also refuses a file that Check finds problems in, since there the
	// rules an import relies on are the file's own triggers. In the Beta
	// the engine keeps the rules, and Check finds no problems until task
	// G7, which settles whether Import runs it here.
	im := importer{tx: tx}
	for {
		item, err := rd.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return importError(err)
		}
		switch x := item.(type) {
		case export.Table:
			err = im.table(x)
		case export.Record:
			err = im.record(x)
		case export.Link:
			err = im.link(x)
		default:
			err = fmt.Errorf("hypercrux: the export's reader gave a %T", item)
		}
		if err != nil {
			return lineError(rd.Line(), err)
		}
	}
}

// holdsTables reports whether r holds a table, even an empty one: then its
// snapshot starts with one. Records and links are in tables, so a database
// without tables holds nothing.
func holdsTables(r store.Reader) bool {
	for range r.Snapshot() {
		return true
	}
	return false
}

// importer puts an export's tables, records and links into a transaction.
// Each check is 0.x's, in 0.x's order, with 0.x's error, so an export that
// breaks several rules at once is refused for the same one by both. A
// table is made whole, and the records and links go through the store's
// Put and a Link change, which keep the rules Put and Link keep.
type importer struct {
	tx *store.Tx
}

// table makes a table with its whole shape, its fields in their order and
// its vector size, in one CreateTable, as FORMAT.md says an import writes
// it. 0.x checks the table's name, then each field's, then the vector
// size. A table in the Beta adds two rules: no more fields than Put lets a
// table have, and a vector size only with a vector field. 0.x takes 1,999
// fields too, and refuses more with SQLite's own error. A name another
// table has taken can't come, since the database held no tables and the
// export's reader refuses a table that's there twice.
func (im importer) table(t export.Table) error {
	if err := rules.Table(t.Name); err != nil {
		return err
	}
	vec := false
	for _, f := range t.Fields {
		if err := rules.Field(f); err != nil {
			return fmt.Errorf("table %s: %w", t.Name, err)
		}
		vec = vec || rules.IsVec(f)
	}
	switch {
	case t.Dims > MaxDims:
		return fmt.Errorf("%w: table %s has a vector size of %d, more than %d", ErrInvalid, t.Name, t.Dims, MaxDims)
	case len(t.Fields) > rules.MaxFields:
		return fmt.Errorf("%w: table %s has %d fields, and a table holds at most %d", ErrInvalid, t.Name, len(t.Fields), rules.MaxFields)
	case t.Dims > 0 && !vec:
		return fmt.Errorf("%w: table %s has a vector size of %d and no field vec, which a table can't have in the Beta; make its dims null", ErrInvalid, t.Name, t.Dims)
	}
	size := t.Dims
	if plant == "hypercrux/import-size-dropped" {
		size = 0
	}
	return im.tx.Apply(format.Change{Op: format.CreateTable, Table: t.Name, Size: size, Names: t.Fields})
}

// record puts a record. Each value goes through Put's conversion,
// value.FromGo, which is what Put does with a Go value, in the table's
// order as 0.x converts them, and then the store's Put checks them again
// and keeps them. 0.x checks the key, then each value, with the vector's
// size against the table's as it comes, and then refuses a record that's
// there twice, where Put would merge the two.
func (im importer) record(r export.Record) error {
	tbl, err := rules.TableOf(r.Key)
	if err != nil {
		return err
	}
	// The table is there: the export's reader takes a record only in a
	// table the export lists, and every table it lists is made first.
	t, _ := im.tx.Table(tbl)
	fields := make([]format.Field, len(r.Fields))
	for i, f := range r.Fields {
		v, err := value.FromGo(f.Name, f.Value)
		if err != nil {
			return fmt.Errorf("%s: %w", r.Key, err)
		}
		if v.Kind() == value.KindVector && t.Size != 0 && v.Dims() != t.Size {
			return fmt.Errorf("%w: %s has a vector of %d values, and table %s holds vectors of %d", ErrInvalid, r.Key, v.Dims(), tbl, t.Size)
		}
		fields[i] = format.Field{Name: f.Name, Value: v}
	}
	if _, err := im.tx.Get(r.Key); err == nil && plant != "hypercrux/import-twice-merged" {
		return fmt.Errorf("%w: the record %s is there twice", ErrInvalid, r.Key)
	}
	return im.tx.Put(r.Key, fields)
}

// link adds a link, in a Link change, which refuses a link that's there
// already, where Link takes it as made and changes nothing. 0.x checks the
// type, then both keys, then that the records exist, the one it's from
// first, and then refuses a link that's there twice, each with the error
// its file's triggers give.
func (im importer) link(l export.Link) error {
	name := l.From + " -" + l.Type + "-> " + l.To
	if err := rules.LinkType(l.Type); err != nil {
		return err
	}
	for _, k := range []string{l.From, l.To} {
		if _, err := rules.TableOf(k); err != nil {
			return fmt.Errorf("the link %s: %w", name, err)
		}
	}
	if _, err := im.tx.Get(l.From); err != nil {
		return fmt.Errorf("%w: the link %s: a link from a key that does not exist", ErrInvalid, name)
	}
	if _, err := im.tx.Get(l.To); err != nil {
		return fmt.Errorf("%w: the link %s: a link to a key that does not exist", ErrInvalid, name)
	}
	if plant == "hypercrux/import-twice-merged" {
		return im.tx.Link(l.From, l.Type, l.To)
	}
	// With the type, the keys and both records checked, the one rule left
	// for a Link change to break is the link being there already.
	err := im.tx.Apply(format.Change{Op: format.Link, Key: l.From, Type: l.Type, To: l.To})
	if errors.Is(err, ErrInvalid) {
		return fmt.Errorf("%w: the link %s is there twice", ErrInvalid, name)
	}
	return err
}

// importError gives an error from the export's reader the kind 0.x gives
// it: input that isn't a valid export wraps ErrInvalid, and reads
// "hypercrux: invalid: not a valid HyperCrux export: " and the reason. Any
// other error, such as one reading r, is as it came.
func importError(err error) error {
	if errors.Is(err, export.ErrFormat) {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return err
}

// lineError names the line of the export that broke a rule, as 0.x does:
// "hypercrux: invalid: line 4 of the export: the record docs:1 is there
// twice". A rule broken wraps ErrInvalid, even one whose error wrapped
// ErrNotFound, and any other error keeps its own kind.
func lineError(line int, err error) error {
	if !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("line %d of the export: %w", line, err)
	}
	return fmt.Errorf("%w: line %d of the export: %s", ErrInvalid, line, strings.ReplaceAll(err.Error(), ErrInvalid.Error()+": ", ""))
}

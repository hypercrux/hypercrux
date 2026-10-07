// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

package hypercrux

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/hypercrux/hypercrux/internal/export"
)

// Export writes every record table, record and link in the file to w as
// JSON lines, in the format EXPORT.md describes. Import reads it back, into
// a new file here or into a later HyperCrux.
//
// Export reads a single moment of the file, so writers carry on while it
// runs. It refuses a file that Check finds problems in, and any value Put
// wouldn't take, such as an infinite number stored with plain SQL, naming
// the record. What it has written by then stops short of the export's last
// line, and Import refuses an export without it.
func (db *DB) Export(w io.Writer) error {
	return db.snapshot(func(conn querier) error {
		rep, err := check(conn, false)
		if err != nil {
			return err
		}
		if !rep.OK() {
			return fmt.Errorf("%w: hypercrux check finds problems in the file, so it isn't exported; the first is: %s", ErrInvalid, rep.Problems[0])
		}
		return exportAll(conn, export.NewWriter(w))
	})
}

func exportAll(q querier, w *export.Writer) error {
	rows, err := q.Query(`SELECT name, dims FROM hc_tables ORDER BY name`)
	if err != nil {
		return err
	}
	var tables []export.Table
	for rows.Next() {
		var t export.Table
		var dims *int64
		if err := rows.Scan(&t.Name, &dims); err != nil {
			rows.Close()
			return err
		}
		if dims != nil {
			t.Dims = int(*dims)
		}
		tables = append(tables, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range tables {
		t := &tables[i]
		cols, err := q.Query(`SELECT name FROM pragma_table_info(?) ORDER BY cid`, t.Name)
		if err != nil {
			return err
		}
		for cols.Next() {
			var name string
			if err := cols.Scan(&name); err != nil {
				cols.Close()
				return err
			}
			if strings.EqualFold(name, "key") {
				continue
			}
			if err := checkField(name); err != nil {
				cols.Close()
				return unexportable("table "+t.Name, err)
			}
			t.Fields = append(t.Fields, name)
		}
		cols.Close()
		if err := cols.Err(); err != nil {
			return err
		}
		if err := w.Table(*t); err != nil {
			return err
		}
	}
	for _, t := range tables {
		if err := exportRecords(q, w, t); err != nil {
			return err
		}
	}
	links, err := q.Query(`SELECT src, type, dst FROM hc_links ORDER BY src, type, dst`)
	if err != nil {
		return err
	}
	defer links.Close()
	for links.Next() {
		var l export.Link
		if err := links.Scan(&l.From, &l.Type, &l.To); err != nil {
			return err
		}
		if err := checkLinkType(l.Type); err != nil {
			return unexportable("the link from "+l.From+" to "+l.To, err)
		}
		if err := w.Link(l); err != nil {
			return err
		}
	}
	if err := links.Err(); err != nil {
		return err
	}
	return w.Close()
}

// exportRecords writes a table's records in key order. Each field is read
// as +field, which gives back the stored value untouched by the column's
// declared type, as go-sqlite3 would otherwise turn a DATETIME column's text
// into a time.Time.
func exportRecords(q querier, w *export.Writer, t export.Table) error {
	var b strings.Builder
	b.WriteString(`SELECT key`)
	for _, f := range t.Fields {
		b.WriteString(", +" + quote(f))
	}
	b.WriteString(` FROM ` + quote(t.Name) + ` ORDER BY key COLLATE BINARY`)
	rows, err := q.Query(b.String())
	if err != nil {
		return err
	}
	defer rows.Close()
	vals := make([]any, 1+len(t.Fields))
	ptrs := make([]any, len(vals))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		key, ok := vals[0].(string)
		if !ok {
			return fmt.Errorf("%w: table %s has a key that isn't text", ErrInvalid, t.Name)
		}
		if _, err := TableOf(key); err != nil {
			return unexportable("table "+t.Name, err)
		}
		rec := export.Record{Key: key}
		for i, f := range t.Fields {
			v := vals[i+1]
			if v == nil {
				continue
			}
			if strings.EqualFold(f, "vec") {
				b, ok := v.([]byte)
				if !ok {
					return fmt.Errorf("%w: the vector of %s isn't a blob", ErrInvalid, key)
				}
				vec, err := DecodeVector(b)
				if err == nil {
					err = checkVector(vec)
				}
				if err != nil {
					return unexportable(key, err)
				}
				v = []float32(vec)
			} else if _, err := encodeField(f, v); err != nil {
				return unexportable(key, err)
			}
			rec.Fields = append(rec.Fields, export.Field{Name: f, Value: v})
		}
		if err := w.Record(rec); err != nil {
			return err
		}
	}
	return rows.Err()
}

// Import reads an export, as Export writes it, into this file, which must
// hold no record tables yet. It all goes in one transaction: if the export
// is cut short, or anything in it breaks the rules Put and Link follow,
// nothing is kept. Tables come back with their fields in the same order and
// the same vector sizes, so exporting the result gives the same bytes.
func (db *DB) Import(r io.Reader) error {
	rd, err := export.NewReader(r)
	if err != nil {
		return importError(err)
	}
	return db.Update(func(tx *Tx) error {
		var n int
		if err := tx.tx.QueryRow(`SELECT (SELECT count(*) FROM hc_tables) + (SELECT count(*) FROM hc_keys) + (SELECT count(*) FROM hc_links)`).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("%w: the file already holds record tables; import into a new file", ErrInvalid)
		}
		im := importer{tx: tx, tables: map[string]*importTable{}}
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
			}
			if err != nil {
				return lineError(rd.Line(), err)
			}
		}
	})
}

func importError(err error) error {
	if errors.Is(err, export.ErrFormat) {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	return err
}

// bare is an error's message without the "hypercrux: invalid: " that
// ErrInvalid puts in front, for wrapping it again.
func bare(err error) string {
	return strings.ReplaceAll(err.Error(), ErrInvalid.Error()+": ", "")
}

// unexportable explains a value Put wouldn't take, which Export refuses.
func unexportable(where string, err error) error {
	return fmt.Errorf("%w: the file can't be exported: %s: %s; Put wouldn't take it, so change it with plain SQL and export again", ErrInvalid, where, bare(err))
}

// lineError adds the export's line number to a broken rule.
func lineError(line int, err error) error {
	if !errors.Is(err, ErrInvalid) && !errors.Is(err, ErrNotFound) {
		return fmt.Errorf("line %d of the export: %w", line, err)
	}
	return fmt.Errorf("%w: line %d of the export: %s", ErrInvalid, line, bare(err))
}

type importTable struct {
	cols map[string]string // a field's column
	dims int
}

type importer struct {
	tx     *Tx
	tables map[string]*importTable
}

func (im *importer) table(t export.Table) error {
	if err := checkTable(t.Name); err != nil {
		return err
	}
	var n int
	if err := im.tx.tx.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name = ? COLLATE NOCASE`, t.Name).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%w: the file already has a table, view or index called %s", ErrInvalid, t.Name)
	}
	for _, f := range t.Fields {
		if err := checkField(f); err != nil {
			return fmt.Errorf("table %s: %w", t.Name, err)
		}
	}
	if t.Dims > MaxDims {
		return fmt.Errorf("%w: table %s has a vector size of %d, more than %d", ErrInvalid, t.Name, t.Dims, MaxDims)
	}
	cols, err := im.tx.ensure(t.Name, t.Fields)
	if err != nil {
		return err
	}
	if t.Dims > 0 {
		if _, err := im.tx.tx.Exec(`UPDATE hc_tables SET dims = ? WHERE name = ?`, t.Dims, t.Name); err != nil {
			return err
		}
	}
	im.tables[t.Name] = &importTable{cols: cols, dims: t.Dims}
	return nil
}

func (im *importer) record(r export.Record) error {
	tbl, err := TableOf(r.Key)
	if err != nil {
		return err
	}
	it := im.tables[tbl]
	var b strings.Builder
	b.WriteString(`INSERT INTO ` + quote(tbl) + ` (key`)
	args := make([]any, 1, 1+len(r.Fields))
	args[0] = r.Key
	for _, f := range r.Fields {
		v, err := encodeField(f.Name, f.Value)
		if err != nil {
			return fmt.Errorf("%s: %w", r.Key, err)
		}
		if vec, ok := f.Value.([]float32); ok {
			if it.dims == 0 {
				it.dims = len(vec)
			} else if len(vec) != it.dims {
				return fmt.Errorf("%w: %s has a vector of %d values, and table %s holds vectors of %d", ErrInvalid, r.Key, len(vec), tbl, it.dims)
			}
		}
		b.WriteString(", " + quote(it.cols[f.Name]))
		args = append(args, v)
	}
	b.WriteString(`) VALUES (?` + strings.Repeat(`, ?`, len(r.Fields)) + `)`)
	if _, err := im.tx.tx.Exec(b.String(), args...); err != nil {
		if isUnique(err) {
			return fmt.Errorf("%w: the record %s is there twice", ErrInvalid, r.Key)
		}
		return fmt.Errorf("%s: %w", r.Key, ruleError(err))
	}
	return nil
}

func (im *importer) link(l export.Link) error {
	name := l.From + " -" + l.Type + "-> " + l.To
	if err := checkLinkType(l.Type); err != nil {
		return err
	}
	for _, k := range []string{l.From, l.To} {
		if _, err := TableOf(k); err != nil {
			return fmt.Errorf("the link %s: %w", name, err)
		}
	}
	_, err := im.tx.tx.Exec(`INSERT INTO hc_links (src, type, dst) VALUES (?, ?, ?)`, l.From, l.Type, l.To)
	if isUnique(err) {
		return fmt.Errorf("%w: the link %s is there twice", ErrInvalid, name)
	}
	if err != nil {
		return fmt.Errorf("the link %s: %w", name, ruleError(err))
	}
	return nil
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

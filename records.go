// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

package hypercrux

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// Fields are a record's values by column name. Put takes these Go types:
// string, bool, every integer type, float32 and float64, []byte, time.Time
// (stored as RFC 3339 text in UTC), nil (stored as NULL), and maps or slices,
// which are stored as JSON text. The field named "vec" is the record's
// vector and takes a Vector or a []float32 or []float64.
//
// Get gives values back as string, int64, float64, []byte, and Vector for
// "vec". Fields that are NULL are left out.
type Fields map[string]any

// Record is a key with its fields.
type Record struct {
	Key    string
	Fields Fields
}

// MaxKeyLen is the longest key HyperCrux accepts, in bytes.
const MaxKeyLen = 1024

var (
	tableName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)
	fieldName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]{0,63}$`)
)

// checkTable checks a record table's name: lower-case letters, digits and
// underscores, starting with a letter, and not one of SQLite's or
// HyperCrux's own prefixes.
func checkTable(t string) error {
	if !tableName.MatchString(t) || strings.HasPrefix(t, "hc_") || strings.HasPrefix(t, "sqlite_") {
		return fmt.Errorf("%w: table name %q: use lower-case letters, digits and underscores, starting with a letter", ErrInvalid, t)
	}
	return nil
}

// TableOf returns the table a key belongs to: the part before the first
// colon. It checks the whole key against HyperCrux's rules.
func TableOf(key string) (string, error) {
	i := strings.IndexByte(key, ':')
	if i < 0 {
		return "", fmt.Errorf("%w: key %q has no table: write it as table:id, such as docs:7", ErrInvalid, key)
	}
	t := key[:i]
	if err := checkTable(t); err != nil {
		return "", fmt.Errorf("key %q: %w", key, err)
	}
	switch {
	case i == len(key)-1:
		return "", fmt.Errorf("%w: key %q has nothing after the colon", ErrInvalid, key)
	case len(key) > MaxKeyLen:
		return "", fmt.Errorf("%w: key is %d bytes, more than %d", ErrInvalid, len(key), MaxKeyLen)
	case !utf8.ValidString(key) || strings.IndexByte(key, 0) >= 0:
		return "", fmt.Errorf("%w: key %q isn't clean UTF-8 text", ErrInvalid, key)
	}
	return t, nil
}

func checkField(name string) error {
	l := strings.ToLower(name)
	if !fieldName.MatchString(name) || l == "key" || l == "rowid" || l == "oid" || l == "_rowid_" {
		return fmt.Errorf("%w: field name %q: use letters, digits and underscores, starting with a letter, and not key or rowid", ErrInvalid, name)
	}
	return nil
}

// encodeField turns a Go value into what Put stores.
func encodeField(name string, v any) (any, error) {
	isVec := strings.EqualFold(name, "vec")
	if isVec && v != nil {
		vec, err := toVector(v)
		if err != nil {
			return nil, err
		}
		if err := checkVector(vec); err != nil {
			return nil, err
		}
		return vec.Bytes(), nil
	}
	switch x := v.(type) {
	case nil:
		return nil, nil
	case string:
		if !utf8.ValidString(x) {
			return nil, fmt.Errorf("%w: field %s isn't valid UTF-8; store bytes as []byte", ErrInvalid, name)
		}
		return x, nil
	case bool:
		if x {
			return int64(1), nil
		}
		return int64(0), nil
	case int:
		return int64(x), nil
	case int8:
		return int64(x), nil
	case int16:
		return int64(x), nil
	case int32:
		return int64(x), nil
	case int64:
		return x, nil
	case uint:
		return uintField(name, uint64(x))
	case uint8:
		return int64(x), nil
	case uint16:
		return int64(x), nil
	case uint32:
		return int64(x), nil
	case uint64:
		return uintField(name, x)
	case float32:
		return floatField(name, float64(x))
	case float64:
		return floatField(name, x)
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i, nil
		}
		f, err := x.Float64()
		if err != nil {
			return nil, fmt.Errorf("%w: field %s: %v", ErrInvalid, name, err)
		}
		return floatField(name, f)
	case []byte:
		return append([]byte{}, x...), nil
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano), nil
	case Vector, []float32, []float64:
		return nil, fmt.Errorf("%w: field %s holds a vector; a record's vector goes in the field vec", ErrInvalid, name)
	case map[string]any, []any:
		b, err := json.Marshal(x)
		if err != nil {
			return nil, fmt.Errorf("%w: field %s: %v", ErrInvalid, name, err)
		}
		return string(b), nil
	}
	return nil, fmt.Errorf("%w: field %s has a %T, which HyperCrux doesn't store", ErrInvalid, name, v)
}

func uintField(name string, x uint64) (any, error) {
	if x > math.MaxInt64 {
		return nil, fmt.Errorf("%w: field %s is %d, beyond SQLite's integers", ErrInvalid, name, x)
	}
	return int64(x), nil
}

func floatField(name string, f float64) (any, error) {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil, fmt.Errorf("%w: field %s is %v", ErrInvalid, name, f)
	}
	return f, nil
}

// Get returns the fields of the record with this key, or ErrNotFound.
func (db *DB) Get(key string) (Fields, error) { return get(db.run(), key) }

// Get is DB.Get inside the transaction.
func (t *Tx) Get(key string) (Fields, error) { return get(t.run(), key) }

func get(q querier, key string) (Fields, error) {
	tbl, err := TableOf(key)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(`SELECT * FROM `+quote(tbl)+` WHERE key = ? AND EXISTS (SELECT 1 FROM hc_tables WHERE name = ?)`, key, tbl)
	if isNoSuchTable(err) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	if err != nil {
		return nil, err
	}
	recs, err := readRecords(rows, true)
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	return recs[0].Fields, nil
}

// readRecords turns rows of a record table into records, and closes them.
func readRecords(rows *sql.Rows, withVec bool) ([]Record, error) {
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	keyAt := -1
	for i, c := range cols {
		if strings.EqualFold(c, "key") {
			keyAt = i
		}
	}
	var out []Record
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		r := Record{Fields: Fields{}}
		for i, c := range cols {
			v := vals[i]
			switch {
			case i == keyAt:
				r.Key = fmt.Sprint(v)
				continue
			case v == nil:
				continue
			case strings.EqualFold(c, "vec"):
				if !withVec {
					continue
				}
				if b, ok := v.([]byte); ok {
					if vec, err := DecodeVector(b); err == nil {
						v = vec
					}
				}
			}
			if b, ok := v.([]byte); ok {
				v = append([]byte{}, b...)
			}
			r.Fields[c] = v
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Put stores fields under key, creating the record if it is new. Fields you
// pass replace those values, fields you leave out keep theirs, and a nil
// value clears a field. The first Put into a table creates the table, and a
// field the table hasn't seen before becomes a new column.
func (db *DB) Put(key string, f Fields) error {
	return db.Update(func(tx *Tx) error { return tx.Put(key, f) })
}

// Put is DB.Put inside the transaction.
func (t *Tx) Put(key string, f Fields) error {
	tbl, err := TableOf(key)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(f))
	seen := map[string]bool{}
	for name := range f {
		if err := checkField(name); err != nil {
			return err
		}
		l := strings.ToLower(name)
		if seen[l] {
			return fmt.Errorf("%w: fields %q and another differ only in case", ErrInvalid, name)
		}
		seen[l] = true
		names = append(names, name)
	}
	sort.Strings(names)
	vals := make([]any, 0, len(names)+1)
	vals = append(vals, key)
	for _, name := range names {
		v, err := encodeField(name, f[name])
		if err != nil {
			return err
		}
		vals = append(vals, v)
	}
	cols, err := t.ensure(tbl, names)
	if err != nil {
		return err
	}
	for i, name := range names {
		if v, ok := vals[i+1].([]byte); ok && strings.EqualFold(name, "vec") {
			var dims *int64
			if err := t.run().QueryRow(`SELECT dims FROM hc_tables WHERE name = ?`, tbl).Scan(&dims); err != nil {
				return err
			}
			if dims != nil && int64(len(v)) != 4**dims {
				return fmt.Errorf("%w: table %s holds vectors of %d values, and %s has %d", ErrInvalid, tbl, *dims, key, len(v)/4)
			}
		}
	}
	var b strings.Builder
	b.WriteString(`INSERT INTO ` + quote(tbl) + ` (key`)
	for _, name := range names {
		b.WriteString(", " + quote(cols[name]))
	}
	b.WriteString(") VALUES (?" + strings.Repeat(", ?", len(names)) + ") ON CONFLICT (key) DO ")
	if len(names) == 0 {
		b.WriteString("NOTHING")
	} else {
		b.WriteString("UPDATE SET ")
		for i, name := range names {
			if i > 0 {
				b.WriteString(", ")
			}
			c := quote(cols[name])
			b.WriteString(c + " = excluded." + c)
		}
	}
	if _, err := t.run().Exec(b.String(), vals...); err != nil {
		return ruleError(err)
	}
	return nil
}

// ruleError turns an error raised by one of HyperCrux's triggers into an
// ErrInvalid, and leaves other errors alone.
func ruleError(err error) error {
	if msg := err.Error(); strings.HasPrefix(msg, "hypercrux: ") {
		return fmt.Errorf("%w: %s", ErrInvalid, strings.TrimPrefix(msg, "hypercrux: "))
	}
	return err
}

// Delete removes the record with this key, and every link to or from it.
// It returns ErrNotFound if there's no such record.
func (db *DB) Delete(key string) error {
	return db.Update(func(tx *Tx) error { return tx.Delete(key) })
}

// Delete is DB.Delete inside the transaction.
func (t *Tx) Delete(key string) error {
	tbl, err := TableOf(key)
	if err != nil {
		return err
	}
	res, err := t.run().Exec(`DELETE FROM `+quote(tbl)+` WHERE key = ? AND EXISTS (SELECT 1 FROM hc_tables WHERE name = ?)`, key, tbl)
	if isNoSuchTable(err) {
		return fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	return nil
}

// Scan returns records whose keys start with prefix, in key order. The
// prefix starts with a table name and a colon, such as "docs:" or
// "docs:2026-". Records come after the key after (use "" to start at the
// beginning), at most limit of them, or all of them when limit is 0.
// Vectors are left out; Get a record to see its vector.
func (db *DB) Scan(prefix, after string, limit int) ([]Record, error) {
	return scan(db.run(), prefix, after, limit)
}

// Scan is DB.Scan inside the transaction.
func (t *Tx) Scan(prefix, after string, limit int) ([]Record, error) {
	return scan(t.run(), prefix, after, limit)
}

func scan(q querier, prefix, after string, limit int) ([]Record, error) {
	i := strings.IndexByte(prefix, ':')
	if i < 0 {
		return nil, fmt.Errorf("%w: scan prefix %q needs a table, such as docs:", ErrInvalid, prefix)
	}
	tbl := prefix[:i]
	if err := checkTable(tbl); err != nil {
		return nil, err
	}
	if limit < 0 {
		return nil, fmt.Errorf("%w: limit %d", ErrInvalid, limit)
	}
	query := `SELECT * FROM ` + quote(tbl) + ` WHERE key >= ? AND key > ?`
	args := []any{prefix, after}
	if end, ok := prefixEnd(prefix); ok {
		query += ` AND key < ?`
		args = append(args, end)
	}
	query += ` AND EXISTS (SELECT 1 FROM hc_tables WHERE name = ?) ORDER BY key`
	args = append(args, tbl)
	if limit > 0 {
		query += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := q.Query(query, args...)
	if isNoSuchTable(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return readRecords(rows, false)
}

// prefixEnd returns the smallest string greater than every string that
// starts with p, in SQLite's byte order.
func prefixEnd(p string) (string, bool) {
	b := []byte(p)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] < 0xff {
			b[i]++
			return string(b[:i+1]), true
		}
	}
	return "", false
}

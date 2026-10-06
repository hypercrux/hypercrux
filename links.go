// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

package hypercrux

import (
	"fmt"
	"unicode/utf8"
)

// Link is a typed, one-way connection between two records.
type Link struct {
	From string
	Type string
	To   string
}

func (l Link) String() string { return l.From + " -" + l.Type + "-> " + l.To }

// Direction picks which links to follow from a record.
type Direction int

const (
	Out  Direction = iota // links from the record to others
	In                    // links from others to the record
	Both                  // either way
)

func (d Direction) String() string {
	switch d {
	case Out:
		return "out"
	case In:
		return "in"
	case Both:
		return "both"
	}
	return fmt.Sprintf("Direction(%d)", int(d))
}

// ParseDirection reads "out", "in" or "both".
func ParseDirection(s string) (Direction, error) {
	switch s {
	case "out", "":
		return Out, nil
	case "in":
		return In, nil
	case "both":
		return Both, nil
	}
	return 0, fmt.Errorf("%w: direction %q: use out, in or both", ErrInvalid, s)
}

// MaxDepth is the furthest Walk goes.
const MaxDepth = 32

func checkLinkType(typ string) error {
	if typ == "" || len(typ) > 200 || !utf8.ValidString(typ) {
		return fmt.Errorf("%w: link type %q: use 1 to 200 characters", ErrInvalid, typ)
	}
	return nil
}

// Link connects from to to with a link of the given type, such as "owns" or
// "cites". Both records must exist. Adding a link that is already there
// does nothing.
func (db *DB) Link(from, typ, to string) error {
	return db.Update(func(tx *Tx) error { return tx.Link(from, typ, to) })
}

// Link is DB.Link inside the transaction.
func (t *Tx) Link(from, typ, to string) error {
	if err := checkLinkType(typ); err != nil {
		return err
	}
	if err := mustExist(t.run(), from, to); err != nil {
		return err
	}
	_, err := t.run().Exec(`INSERT OR IGNORE INTO hc_links (src, type, dst) VALUES (?, ?, ?)`, from, typ, to)
	return err
}

func mustExist(q querier, keys ...string) error {
	for _, k := range keys {
		if _, err := TableOf(k); err != nil {
			return err
		}
		var n int
		if err := q.QueryRow(`SELECT count(*) FROM hc_keys WHERE key = ?`, k).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("%w: %s", ErrNotFound, k)
		}
	}
	return nil
}

// Unlink removes the link of the given type from from to to, or every link
// from from to to when typ is "". It returns ErrNotFound if there was none.
func (db *DB) Unlink(from, typ, to string) error {
	return db.Update(func(tx *Tx) error { return tx.Unlink(from, typ, to) })
}

// Unlink is DB.Unlink inside the transaction.
func (t *Tx) Unlink(from, typ, to string) error {
	q, args := `DELETE FROM hc_links WHERE src = ? AND dst = ?`, []any{from, to}
	if typ != "" {
		q += ` AND type = ?`
		args = append(args, typ)
	}
	res, err := t.run().Exec(q, args...)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return fmt.Errorf("%w: no link from %s to %s", ErrNotFound, from, to)
	}
	return nil
}

// Neighbours returns the links of the record with this key, out of it, into
// it, or both, sorted by type and then key. typ "" means links of every
// type. It returns ErrNotFound if there's no such record.
func (db *DB) Neighbours(key string, dir Direction, typ string) ([]Link, error) {
	return neighbours(db.run(), key, dir, typ)
}

// Neighbours is DB.Neighbours inside the transaction.
func (t *Tx) Neighbours(key string, dir Direction, typ string) ([]Link, error) {
	return neighbours(t.run(), key, dir, typ)
}

func neighbours(q querier, key string, dir Direction, typ string) ([]Link, error) {
	if err := mustExist(q, key); err != nil {
		return nil, err
	}
	const out = `SELECT src, type, dst FROM hc_links WHERE src = ?1 AND (?2 = '' OR type = ?2)`
	const in = `SELECT src, type, dst FROM hc_links WHERE dst = ?1 AND (?2 = '' OR type = ?2)`
	var query string
	switch dir {
	case Out:
		query = out + ` ORDER BY type, dst`
	case In:
		query = in + ` ORDER BY type, src`
	case Both:
		query = out + ` UNION ` + in + ` ORDER BY 2, 1, 3`
	default:
		return nil, fmt.Errorf("%w: direction %d", ErrInvalid, dir)
	}
	rows, err := q.Query(query, key, typ)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var links []Link
	for rows.Next() {
		var l Link
		if err := rows.Scan(&l.From, &l.Type, &l.To); err != nil {
			return nil, err
		}
		links = append(links, l)
	}
	return links, rows.Err()
}

// Step is one record Walk reached, and how many links it took to get there.
type Step struct {
	Key   string
	Depth int
}

// Walk follows links from the record with this key, up to depth links away,
// and returns every record it reaches with the fewest links needed, nearest
// first. The record itself isn't included. typ "" follows links of every
// type. depth runs from 1 to MaxDepth.
func (db *DB) Walk(key string, dir Direction, typ string, depth int) ([]Step, error) {
	return walk(db.run(), key, dir, typ, depth)
}

// Walk is DB.Walk inside the transaction.
func (t *Tx) Walk(key string, dir Direction, typ string, depth int) ([]Step, error) {
	return walk(t.run(), key, dir, typ, depth)
}

func walk(q querier, key string, dir Direction, typ string, depth int) ([]Step, error) {
	query, err := walkSQL(dir, depth)
	if err != nil {
		return nil, err
	}
	if err := mustExist(q, key); err != nil {
		return nil, err
	}
	rows, err := q.Query(query, key, depth, typ)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var steps []Step
	for rows.Next() {
		var s Step
		if err := rows.Scan(&s.Key, &s.Depth); err != nil {
			return nil, err
		}
		steps = append(steps, s)
	}
	return steps, rows.Err()
}

// walkSQL is the recursive query behind Walk and the SQL function walk().
// Its parameters are the start key, the depth and the link type (an empty
// string for any). UNION keeps each key once per depth, so cycles end at the depth
// limit.
func walkSQL(dir Direction, depth int) (string, error) {
	if depth < 1 || depth > MaxDepth {
		return "", fmt.Errorf("%w: depth is from 1 to %d, not %d", ErrInvalid, MaxDepth, depth)
	}
	const outStep = `
	SELECT l.dst, w.depth + 1 FROM w JOIN hc_links l ON l.src = w.key
	WHERE w.depth < ?2 AND (?3 = '' OR l.type = ?3)`
	const inStep = `
	SELECT l.src, w.depth + 1 FROM w JOIN hc_links l ON l.dst = w.key
	WHERE w.depth < ?2 AND (?3 = '' OR l.type = ?3)`
	var steps string
	switch dir {
	case Out:
		steps = outStep
	case In:
		steps = inStep
	case Both:
		steps = outStep + "\n\tUNION" + inStep
	default:
		return "", fmt.Errorf("%w: direction %d", ErrInvalid, dir)
	}
	return `WITH RECURSIVE w(key, depth) AS (
	SELECT ?1, 0
	UNION` + steps + `
)
SELECT key, min(depth) FROM w WHERE key <> ?1 GROUP BY key ORDER BY 2, 1`, nil
}

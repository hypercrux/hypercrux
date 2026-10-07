// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

package hypercrux

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"strings"
)

var bg = context.Background()

// connQuerier lets a single connection stand in where a querier is needed.
type connQuerier struct{ c *sql.Conn }

func (q connQuerier) Exec(query string, args ...any) (sql.Result, error) {
	return q.c.ExecContext(bg, query, args...)
}

func (q connQuerier) Query(query string, args ...any) (*sql.Rows, error) {
	return q.c.QueryContext(bg, query, args...)
}

func (q connQuerier) QueryRow(query string, args ...any) *sql.Row {
	return q.c.QueryRowContext(bg, query, args...)
}

// Report is what Check found.
type Report struct {
	Tables   int      // record tables
	Records  int      // records in them
	Links    int      // links between records
	Vectors  int      // records with a vector
	Problems []string // empty when the file is consistent
}

// OK reports whether Check found no problems.
func (r Report) OK() bool { return len(r.Problems) == 0 }

// maxProblems caps how many problems of one kind a report lists.
const maxProblems = 20

// Check reads the whole file and confirms that the four ways into it agree:
// SQLite's integrity check passes, every row of a record table is in the key
// registry and every registered key has its row, every key starts with its
// table's name, every link joins two records that exist, every vector has
// its table's size and finite values, and every record table has its
// triggers. It runs in one read transaction, so it sees a single moment.
func (db *DB) Check() (Report, error) {
	var rep Report
	err := db.snapshot(func(conn querier) error {
		var err error
		rep, err = check(conn, true)
		return err
	})
	return rep, err
}

// snapshot runs fn on one connection inside a read transaction, so that
// everything fn reads comes from a single moment.
func (db *DB) snapshot(fn func(conn querier) error) error {
	c, err := db.sql.Conn(bg)
	if err != nil {
		return err
	}
	defer c.Close()
	conn := connQuerier{c}
	if _, err := conn.Exec(`BEGIN DEFERRED`); err != nil {
		return err
	}
	defer conn.Exec(`ROLLBACK`)
	return fn(conn)
}

// check is Check inside a snapshot. Export runs it without SQLite's
// integrity check, which reads every page of the file.
func check(conn querier, integrity bool) (Report, error) {
	var rep Report
	problem := func(format string, args ...any) {
		rep.Problems = append(rep.Problems, fmt.Sprintf(format, args...))
	}
	if integrity {
		var lines []string
		rows, err := conn.Query(`PRAGMA integrity_check`)
		if err != nil {
			return rep, err
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				rows.Close()
				return rep, err
			}
			lines = append(lines, s)
		}
		rows.Close()
		if len(lines) != 1 || lines[0] != "ok" {
			for i, s := range lines {
				if i == maxProblems {
					break
				}
				problem("SQLite integrity check: %s", s)
			}
		}
	}

	type tbl struct {
		name string
		dims *int64
	}
	var tables []tbl
	rows, err := conn.Query(`SELECT name, dims FROM hc_tables ORDER BY name`)
	if err != nil {
		return rep, err
	}
	for rows.Next() {
		var t tbl
		if err := rows.Scan(&t.name, &t.dims); err != nil {
			rows.Close()
			return rep, err
		}
		tables = append(tables, t)
	}
	rows.Close()
	rep.Tables = len(tables)

	// list runs a query that should return nothing, and reports what it does.
	list := func(what, query string, args ...any) error {
		rows, err := conn.Query(query+` ORDER BY 1`, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		n := 0
		var shown []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return err
			}
			if n < 5 {
				shown = append(shown, s)
			}
			n++
		}
		if n > 0 {
			more := ""
			if n > len(shown) {
				more = fmt.Sprintf(" and %d more", n-len(shown))
			}
			problem("%s: %s%s", what, strings.Join(shown, ", "), more)
		}
		return rows.Err()
	}

	var trig []string
	trows, err := conn.Query(`SELECT name FROM sqlite_schema WHERE type = 'trigger'`)
	if err != nil {
		return rep, err
	}
	have := map[string]bool{}
	for trows.Next() {
		var name string
		if err := trows.Scan(&name); err != nil {
			trows.Close()
			return rep, err
		}
		have[name] = true
	}
	trows.Close()
	for _, name := range []string{"hc_links_insert", "hc_links_update", "hc_keys_delete", "hc_keys_update"} {
		if !have[name] {
			trig = append(trig, name)
		}
	}
	for _, name := range trig {
		problem("the file is missing the trigger %s", name)
	}

	known := map[string]bool{}
	for _, t := range tables {
		known[t.name] = true
		if err := checkTable(t.name); err != nil {
			problem("hc_tables lists %q, which isn't a valid table name", t.name)
			continue
		}
		ti, err := readTableInfo(conn, t.name)
		if err != nil {
			return rep, err
		}
		if !ti.exists {
			problem("hc_tables lists %s, but there's no such table; Drop clears what's left of it", t.name)
			continue
		}
		if !ti.keyOK {
			problem("table %s has no key TEXT PRIMARY KEY column", t.name)
			continue
		}
		for _, trig := range triggerNames(t.name, ti.hasVec()) {
			if !ti.triggers[trig] {
				problem("table %s is missing its trigger %s", t.name, trig)
			}
		}
		q := quote(t.name)
		var n int
		if err := conn.QueryRow(`SELECT count(*) FROM ` + q).Scan(&n); err != nil {
			return rep, err
		}
		rep.Records += n
		prefix := t.name + ":"
		if err := list("keys in "+t.name+" that don't start with "+prefix,
			`SELECT quote(key) FROM `+q+` WHERE typeof(key) <> 'text' OR length(key) <= ? OR substr(key, 1, ?) <> ?`,
			len(prefix), len(prefix), prefix); err != nil {
			return rep, err
		}
		if err := list("rows in "+t.name+" missing from hc_keys",
			`SELECT key FROM `+q+` WHERE key NOT IN (SELECT key FROM hc_keys WHERE tbl = ?)`, t.name); err != nil {
			return rep, err
		}
		if err := list("keys in hc_keys with no row in "+t.name,
			`SELECT key FROM hc_keys WHERE tbl = ? AND key NOT IN (SELECT key FROM `+q+`)`, t.name); err != nil {
			return rep, err
		}
		if ti.hasVec() {
			if err := checkVectors(conn, &rep, t.name, ti.cols["vec"], t.dims, problem); err != nil {
				return rep, err
			}
		}
	}
	if err := list("keys in hc_keys for tables that aren't record tables",
		`SELECT key FROM hc_keys WHERE tbl NOT IN (SELECT name FROM hc_tables)`); err != nil {
		return rep, err
	}
	if err := list("links from keys that don't exist",
		`SELECT src FROM hc_links WHERE src NOT IN (SELECT key FROM hc_keys)`); err != nil {
		return rep, err
	}
	if err := list("links to keys that don't exist",
		`SELECT dst FROM hc_links WHERE dst NOT IN (SELECT key FROM hc_keys)`); err != nil {
		return rep, err
	}
	if err := list("links with a bad type",
		`SELECT src FROM hc_links WHERE typeof(type) <> 'text' OR length(type) NOT BETWEEN 1 AND 200`); err != nil {
		return rep, err
	}
	if err := conn.QueryRow(`SELECT count(*) FROM hc_links`).Scan(&rep.Links); err != nil {
		return rep, err
	}
	return rep, nil
}

// checkVectors reads every vector in a table and checks it.
func checkVectors(conn querier, rep *Report, table, col string, dims *int64, problem func(string, ...any)) error {
	rows, err := conn.Query(`SELECT key, ` + quote(col) + ` FROM ` + quote(table) + ` WHERE ` + quote(col) + ` IS NOT NULL`)
	if err != nil {
		return err
	}
	defer rows.Close()
	bad := 0
	for rows.Next() {
		var key string
		var raw any
		if err := rows.Scan(&key, &raw); err != nil {
			return err
		}
		rep.Vectors++
		why := ""
		b, isBlob := raw.([]byte)
		switch {
		case !isBlob:
			why = "isn't a blob"
		case dims == nil:
			why = "is there, but the table has no vector size recorded"
		case int64(len(b)) != 4**dims:
			why = fmt.Sprintf("has %d bytes, and the table's vectors have %d values", len(b), *dims)
		default:
			v, _ := DecodeVector(b)
			zero := true
			for _, x := range v {
				if math.IsNaN(float64(x)) || math.IsInf(float64(x), 0) {
					why = "has a value that isn't a finite number"
					break
				}
				if x != 0 {
					zero = false
				}
			}
			if why == "" && zero {
				why = "is all zeros"
			}
		}
		if why != "" {
			bad++
			if bad <= maxProblems {
				problem("the vector of %s %s", key, why)
			}
		}
	}
	if bad > maxProblems {
		problem("and %d more bad vectors in %s", bad-maxProblems, table)
	}
	return rows.Err()
}

// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"unicode/utf8"

	"github.com/hypercrux/hypercrux"
)

// returnsRows guesses whether a statement gives rows back.
var returnsRows = regexp.MustCompile(`(?is)^\s*(select|with|values|pragma|explain)\b|\breturning\b`)

// sqlArg turns a command-line argument into an SQL value: whole numbers and
// decimals become numbers, everything else stays text.
func sqlArg(s string) any {
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		return i
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && !strings.ContainsAny(s, "nNiI") {
		return f
	}
	return s
}

func runSQL(db *hypercrux.DB, o *options, w io.Writer) error {
	if len(o.pos) < 2 {
		return usageErr("expected FILE STATEMENT [ARG...]")
	}
	stmt := o.pos[1]
	args := make([]any, 0, len(o.pos)-2)
	for _, a := range o.pos[2:] {
		args = append(args, sqlArg(a))
	}
	if !returnsRows.MatchString(stmt) {
		res, err := db.Exec(stmt, args...)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		fmt.Fprintf(w, "%d rows changed\n", n)
		return nil
	}
	rows, err := db.Query(stmt, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if !o.json {
		fmt.Fprintln(tw, strings.Join(cols, "\t"))
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		if o.json {
			var b bytes.Buffer
			b.WriteByte('{')
			for i, c := range cols {
				if i > 0 {
					b.WriteByte(',')
				}
				writeJSON(&b, c)
				b.WriteByte(':')
				writeJSON(&b, jsonValue(c, vals[i]))
			}
			b.WriteByte('}')
			fmt.Fprintln(w, b.String())
			continue
		}
		cells := make([]string, len(cols))
		for i, c := range cols {
			cells[i] = cell(c, vals[i])
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return tw.Flush()
}

// asVector reads a vec column's blob as a vector, if it is one.
func asVector(col string, v any) (hypercrux.Vector, bool) {
	b, ok := v.([]byte)
	if !ok || !strings.EqualFold(col, "vec") {
		return nil, false
	}
	vec, err := hypercrux.DecodeVector(b)
	return vec, err == nil
}

func jsonValue(col string, v any) any {
	if vec, ok := asVector(col, v); ok {
		return []float32(vec)
	}
	return v
}

// cell formats a value for the table view.
func cell(col string, v any) string {
	switch x := v.(type) {
	case nil:
		return "NULL"
	case []byte:
		if vec, ok := asVector(col, x); ok {
			return fmt.Sprintf("vector(%d)", len(vec))
		}
		if utf8.Valid(x) && len(x) <= 80 && !bytes.ContainsAny(x, "\x00\t\n") {
			return string(x)
		}
		return fmt.Sprintf("<%d bytes>", len(x))
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	case string:
		return strings.NewReplacer("\t", " ", "\n", " ").Replace(x)
	}
	return fmt.Sprint(v)
}

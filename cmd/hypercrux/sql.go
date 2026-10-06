// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"unicode/utf8"

	"github.com/hypercrux/hypercrux"
)

// isTrigger matches a CREATE TRIGGER statement, whose body has semicolons
// of its own.
var isTrigger = regexp.MustCompile(`(?is)^(\s|--[^\n]*(\n|$)|/\*.*?\*/)*create\s+(temp\s+|temporary\s+)?trigger\b`)

// oneStatement checks that s holds exactly one SQL statement, so nothing
// runs that the reader of the command line didn't see, and returns it
// without the semicolon and anything after it.
func oneStatement(s string) (string, error) {
	if isTrigger.MatchString(s) {
		return s, nil
	}
	content, ended, end := false, false, len(s)
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '-' && i+1 < len(s) && s[i+1] == '-':
			if j := strings.IndexByte(s[i:], '\n'); j >= 0 {
				i += j + 1
			} else {
				i = len(s)
			}
			continue
		case c == '/' && i+1 < len(s) && s[i+1] == '*':
			if j := strings.Index(s[i+2:], "*/"); j >= 0 {
				i += j + 4
			} else {
				i = len(s)
			}
			continue
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f':
			i++
			continue
		}
		if ended {
			return "", usageErr("sql runs one statement at a time")
		}
		switch c {
		case '\'', '"', '`':
			j := i + 1
			for j < len(s) {
				if s[j] == c {
					if j+1 < len(s) && s[j+1] == c {
						j += 2
						continue
					}
					break
				}
				j++
			}
			i = j + 1
		case '[':
			if j := strings.IndexByte(s[i:], ']'); j >= 0 {
				i += j + 1
			} else {
				i = len(s)
			}
		case ';':
			ended, end = true, i
			i++
			continue
		default:
			i++
		}
		content = true
	}
	if !content {
		return "", usageErr("sql needs a statement")
	}
	return s[:end], nil
}

// sqlArg turns a command-line argument into an SQL value. A number stays a
// number only when it reads back the same way, so 0501234567 stays text.
func sqlArg(s string) any {
	if i, err := strconv.ParseInt(s, 10, 64); err == nil && strconv.FormatInt(i, 10) == s {
		return i
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil && strconv.FormatFloat(f, 'f', -1, 64) == s {
		return f
	}
	return s
}

func runSQL(db *hypercrux.DB, o *options, w io.Writer) error {
	if len(o.pos) < 2 {
		return usageErr("expected FILE STATEMENT [ARG...]")
	}
	stmt, err := oneStatement(o.pos[1])
	if err != nil {
		return err
	}
	args := make([]any, 0, len(o.pos)-2)
	for _, a := range o.pos[2:] {
		args = append(args, sqlArg(a))
	}
	// One connection, so the change count afterwards is this statement's.
	ctx := context.Background()
	conn, err := db.SQL().Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	var before int64
	if err := conn.QueryRowContext(ctx, `SELECT total_changes()`).Scan(&before); err != nil {
		return err
	}
	rows, err := conn.QueryContext(ctx, stmt, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	if len(cols) == 0 {
		for rows.Next() {
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		var after, n int64
		if err := conn.QueryRowContext(ctx, `SELECT total_changes(), changes()`).Scan(&after, &n); err != nil {
			return err
		}
		if after == before {
			n = 0 // a statement such as CREATE that changes no rows
		}
		fmt.Fprintf(w, "%d rows changed\n", n)
		return nil
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

// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

// Command hypercrux reads and writes HyperCrux files from the command line:
// records by key, plain SQL, links and nearest-vector search.
package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/hypercrux/hypercrux"
	sqlite3 "github.com/mattn/go-sqlite3"
)

const usage = `hypercrux: one record, four handles: key, SQL, links, similarity.

Usage:
  hypercrux init FILE                         create an empty HyperCrux file
  hypercrux put FILE KEY [JSON]               store fields, from JSON or standard input
  hypercrux get FILE KEY                      print a record as JSON
  hypercrux delete FILE KEY                   delete a record and its links
  hypercrux scan FILE PREFIX                  list records by key prefix
                                              --after KEY  --limit N  --vec
  hypercrux sql [--json] FILE STATEMENT [ARG...]
                                              run one SQL statement with ? arguments
  hypercrux link FILE FROM TYPE TO            link two records
  hypercrux unlink FILE FROM TYPE TO          remove a link (TYPE * for every type)
  hypercrux neighbours FILE KEY               links of a record  --in --both --type T --json
  hypercrux walk FILE KEY DEPTH               records up to DEPTH links away
                                              --in --both --type T --json
  hypercrux nearest FILE TABLE VECTOR|KEY     closest vectors  -k N --where SQL --json
  hypercrux adopt FILE TABLE                  make a plain SQL table a record table,
                                              or bring one back in step after a schema change
  hypercrux drop FILE TABLE                   delete a record table, its records and their links
  hypercrux check FILE                        confirm keys, rows, links and vectors agree
  hypercrux version                           print the version

A key is table:id, such as docs:7. A VECTOR is a JSON array, such as
'[0.12, 0.8, 0.05]'. Options can go before or after the other arguments,
except with sql, whose options go before FILE: everything after FILE is the
statement and its arguments. Only init and put create a file.`

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

// errUsage marks a mistake in how the command was called.
type errUsage struct{ msg string }

func (e errUsage) Error() string { return e.msg }

func usageErr(format string, args ...any) error { return errUsage{fmt.Sprintf(format, args...)} }

type options struct {
	pos               []string
	after, typ, where string
	limit, k          int
	json, vec         bool
	dir               hypercrux.Direction
}

// valued lists the options that take a value.
var valued = map[string]bool{"after": true, "limit": true, "type": true, "k": true, "where": true}

// parseArgs reads options and positional arguments. Once rawAfter
// positional arguments have been read (when rawAfter > 0), everything else
// is positional, even if it starts with a dash.
func parseArgs(args []string, rawAfter int) (*options, error) {
	o := &options{k: 10}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" || (rawAfter > 0 && len(o.pos) >= rawAfter) {
			if a == "--" {
				i++
			}
			o.pos = append(o.pos, args[i:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			o.pos = append(o.pos, a)
			continue
		}
		if _, err := strconv.ParseFloat(a, 64); err == nil {
			o.pos = append(o.pos, a) // a negative number, as an SQL argument
			continue
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if valued[name] && !hasVal {
			if i+1 >= len(args) {
				return nil, usageErr("option %s needs a value", a)
			}
			i++
			val = args[i]
		}
		var err error
		switch name {
		case "after":
			o.after = val
		case "type":
			o.typ = val
		case "where":
			o.where = val
		case "limit":
			if o.limit, err = strconv.Atoi(val); err != nil || o.limit < 0 {
				return nil, usageErr("--limit takes a whole number, not %q", val)
			}
		case "k":
			if o.k, err = strconv.Atoi(val); err != nil || o.k < 1 {
				return nil, usageErr("-k takes a whole number from 1, not %q", val)
			}
		case "json":
			o.json = true
		case "vec":
			o.vec = true
		case "in":
			o.dir = hypercrux.In
		case "both":
			o.dir = hypercrux.Both
		case "out":
			o.dir = hypercrux.Out
		case "h", "help":
			return nil, errUsage{}
		default:
			return nil, usageErr("unknown option %s", a)
		}
	}
	return o, nil
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	cmd := args[0]
	rawAfter := 0
	if cmd == "sql" {
		rawAfter = 1 // after FILE come the statement and its arguments, as they are
	}
	o, err := parseArgs(args[1:], rawAfter)
	if err == nil {
		err = dispatch(cmd, o, stdin, stdout)
	}
	var ue errUsage
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ue):
		if ue.msg != "" {
			fmt.Fprintln(stderr, "hypercrux:", ue.msg)
			fmt.Fprintln(stderr, "Run hypercrux help for the commands.")
		} else {
			fmt.Fprintln(stdout, usage)
			return 0
		}
		return 2
	case errors.Is(err, errProblems):
		return 1
	default:
		msg := err.Error()
		if !strings.HasPrefix(msg, "hypercrux:") {
			msg = "hypercrux: " + msg
		}
		fmt.Fprintln(stderr, msg)
		return 1
	}
}

// errProblems means check found problems and has already listed them.
var errProblems = errors.New("problems found")

// need checks the number of arguments after the command.
func need(o *options, n int, names string) error {
	if len(o.pos) != n {
		return usageErr("expected %s", names)
	}
	return nil
}

func dispatch(cmd string, o *options, stdin io.Reader, stdout io.Writer) error {
	switch cmd {
	case "help", "-h", "--help":
		return errUsage{}
	case "version", "--version":
		lib, _, _ := sqlite3.Version()
		fmt.Fprintf(stdout, "hypercrux %s, file format %d, SQLite %s\n", hypercrux.Version, hypercrux.FormatVersion, lib)
		return nil
	case "init":
		if err := need(o, 1, "FILE"); err != nil {
			return err
		}
		db, err := hypercrux.Open(o.pos[0])
		if err != nil {
			return err
		}
		defer db.Close()
		fmt.Fprintf(stdout, "%s is ready (HyperCrux file format %d)\n", o.pos[0], hypercrux.FormatVersion)
		return nil
	case "put":
		if len(o.pos) != 2 && len(o.pos) != 3 {
			return usageErr("expected FILE KEY [JSON]")
		}
		return put(o, stdin, stdout)
	}

	commands := map[string]func(*hypercrux.DB, *options, io.Writer) error{
		"get": get, "delete": del, "scan": scan, "sql": runSQL, "link": link, "unlink": unlink,
		"neighbours": neighbours, "neighbors": neighbours, "walk": walk, "nearest": nearest,
		"adopt": adopt, "drop": drop, "check": check,
	}
	f, ok := commands[cmd]
	if !ok {
		return usageErr("unknown command %q", cmd)
	}
	if len(o.pos) == 0 {
		return usageErr("expected FILE after %s", cmd)
	}
	db, err := openExisting(o.pos[0])
	if err != nil {
		return err
	}
	defer db.Close()
	return f(db, o, stdout)
}

// openExisting opens a file that must already exist and already be a
// HyperCrux file, so a typo in a file name doesn't create an empty one and
// other SQLite files are left alone.
func openExisting(path string) (*hypercrux.DB, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("%s: no such file (only init and put create one)", path)
	}
	if st.IsDir() {
		return nil, fmt.Errorf("%s is a directory", path)
	}
	plain, err := sql.Open(hypercrux.DriverName, path)
	if err != nil {
		return nil, err
	}
	var n int
	err = plain.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = 'hc_meta'`).Scan(&n)
	plain.Close()
	if err != nil {
		return nil, fmt.Errorf("%s: %v", path, err)
	}
	if n == 0 {
		return nil, fmt.Errorf("%s isn't a HyperCrux file; run hypercrux init %s to add HyperCrux's tables to it", path, path)
	}
	return hypercrux.Open(path)
}

func put(o *options, stdin io.Reader, stdout io.Writer) error {
	path, key := o.pos[0], o.pos[1]
	var src io.Reader = stdin
	if len(o.pos) == 3 {
		src = strings.NewReader(o.pos[2])
	}
	dec := json.NewDecoder(src)
	dec.UseNumber()
	var f map[string]any
	if err := dec.Decode(&f); err != nil {
		return fmt.Errorf("the fields must be a JSON object: %v", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return fmt.Errorf("the fields must be one JSON object, with nothing after it")
	}
	if k, ok := f["key"]; ok {
		if k != key {
			return fmt.Errorf("the JSON has key %v, and the command says %s", k, key)
		}
		delete(f, "key")
	}
	if _, err := hypercrux.TableOf(key); err != nil {
		return err
	}
	_, statErr := os.Stat(path)
	created := os.IsNotExist(statErr)
	db, err := hypercrux.Open(path)
	if err != nil {
		return err
	}
	if err := db.Put(key, hypercrux.Fields(f)); err != nil {
		db.Close()
		if created { // don't leave an empty file behind
			for _, p := range []string{path, path + "-wal", path + "-shm"} {
				os.Remove(p)
			}
		}
		return err
	}
	db.Close()
	fmt.Fprintln(stdout, key)
	return nil
}

func get(db *hypercrux.DB, o *options, w io.Writer) error {
	if err := need(o, 2, "FILE KEY"); err != nil {
		return err
	}
	f, err := db.Get(o.pos[1])
	if err != nil {
		return err
	}
	fmt.Fprintln(w, string(recordJSON(o.pos[1], f, true)))
	return nil
}

// recordJSON writes a record as a JSON object with its key first. Pretty
// puts each field on a line of its own, with the value kept on one line.
func recordJSON(key string, f hypercrux.Fields, pretty bool) []byte {
	names := make([]string, 0, len(f))
	for n := range f {
		names = append(names, n)
	}
	sort.Strings(names)
	sep, colon, end := ",", ":", "}"
	var b bytes.Buffer
	b.WriteString("{")
	if pretty {
		sep, colon, end = ",\n  ", ": ", "\n}"
		b.WriteString("\n  ")
	}
	writeJSON(&b, "key")
	b.WriteString(colon)
	writeJSON(&b, key)
	for _, n := range names {
		b.WriteString(sep)
		writeJSON(&b, n)
		b.WriteString(colon)
		writeJSON(&b, f[n])
	}
	b.WriteString(end)
	return b.Bytes()
}

func writeJSON(b *bytes.Buffer, v any) {
	if vec, ok := v.(hypercrux.Vector); ok {
		v = []float32(vec)
	}
	j, err := json.Marshal(v)
	if err != nil {
		j, _ = json.Marshal(fmt.Sprint(v))
	}
	b.Write(j)
}

func del(db *hypercrux.DB, o *options, w io.Writer) error {
	if err := need(o, 2, "FILE KEY"); err != nil {
		return err
	}
	if err := db.Delete(o.pos[1]); err != nil {
		return err
	}
	fmt.Fprintln(w, "deleted", o.pos[1])
	return nil
}

func scan(db *hypercrux.DB, o *options, w io.Writer) error {
	if err := need(o, 2, "FILE PREFIX"); err != nil {
		return err
	}
	recs, err := db.Scan(o.pos[1], o.after, o.limit)
	if err != nil {
		return err
	}
	for _, r := range recs {
		if o.vec {
			if full, err := db.Get(r.Key); err == nil {
				r.Fields = full
			}
		}
		fmt.Fprintln(w, string(recordJSON(r.Key, r.Fields, false)))
	}
	return nil
}

func link(db *hypercrux.DB, o *options, w io.Writer) error {
	if err := need(o, 4, "FILE FROM TYPE TO"); err != nil {
		return err
	}
	l := hypercrux.Link{From: o.pos[1], Type: o.pos[2], To: o.pos[3]}
	if err := db.Link(l.From, l.Type, l.To); err != nil {
		return err
	}
	fmt.Fprintln(w, l)
	return nil
}

func unlink(db *hypercrux.DB, o *options, w io.Writer) error {
	if err := need(o, 4, "FILE FROM TYPE TO"); err != nil {
		return err
	}
	typ := o.pos[2]
	if typ == "*" {
		typ = ""
	}
	if err := db.Unlink(o.pos[1], typ, o.pos[3]); err != nil {
		return err
	}
	fmt.Fprintln(w, "unlinked", o.pos[1], "-"+o.pos[2]+"->", o.pos[3])
	return nil
}

func neighbours(db *hypercrux.DB, o *options, w io.Writer) error {
	if err := need(o, 2, "FILE KEY"); err != nil {
		return err
	}
	links, err := db.Neighbours(o.pos[1], o.dir, o.typ)
	if err != nil {
		return err
	}
	for _, l := range links {
		if o.json {
			b, _ := json.Marshal(map[string]string{"from": l.From, "type": l.Type, "to": l.To})
			fmt.Fprintln(w, string(b))
		} else {
			fmt.Fprintln(w, l)
		}
	}
	return nil
}

func walk(db *hypercrux.DB, o *options, w io.Writer) error {
	if err := need(o, 3, "FILE KEY DEPTH"); err != nil {
		return err
	}
	depth, err := strconv.Atoi(o.pos[2])
	if err != nil {
		return usageErr("DEPTH is a whole number of links, not %q", o.pos[2])
	}
	steps, err := db.Walk(o.pos[1], o.dir, o.typ, depth)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, s := range steps {
		if o.json {
			b, _ := json.Marshal(map[string]any{"key": s.Key, "depth": s.Depth})
			fmt.Fprintln(w, string(b))
		} else {
			fmt.Fprintf(tw, "%d\t%s\n", s.Depth, s.Key)
		}
	}
	return tw.Flush()
}

func nearest(db *hypercrux.DB, o *options, w io.Writer) error {
	if err := need(o, 3, "FILE TABLE VECTOR|KEY"); err != nil {
		return err
	}
	table, src := o.pos[1], o.pos[2]
	var q hypercrux.Vector
	skip := ""
	if strings.HasPrefix(strings.TrimSpace(src), "[") {
		v, err := hypercrux.ParseVector(src)
		if err != nil {
			return err
		}
		q = v
	} else {
		f, err := db.Get(src)
		if err != nil {
			return err
		}
		v, ok := f["vec"].(hypercrux.Vector)
		if !ok {
			return fmt.Errorf("%s has no vector", src)
		}
		q, skip = v, src
	}
	k := o.k
	if skip != "" && k < hypercrux.MaxK {
		k++
	}
	hits, err := db.Nearest(table, q, k, o.where)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	shown := 0
	for _, h := range hits {
		if h.Key == skip || shown == o.k {
			continue
		}
		shown++
		if o.json {
			b, _ := json.Marshal(map[string]any{"key": h.Key, "distance": h.Distance})
			fmt.Fprintln(w, string(b))
		} else {
			fmt.Fprintf(tw, "%.4f\t%s\n", h.Distance, h.Key)
		}
	}
	return tw.Flush()
}

func adopt(db *hypercrux.DB, o *options, w io.Writer) error {
	if err := need(o, 2, "FILE TABLE"); err != nil {
		return err
	}
	if err := db.Adopt(o.pos[1]); err != nil {
		return err
	}
	var n int
	db.SQL().QueryRow(`SELECT count(*) FROM hc_keys WHERE tbl = ?`, o.pos[1]).Scan(&n)
	fmt.Fprintf(w, "%s is a record table with %d records\n", o.pos[1], n)
	return nil
}

func drop(db *hypercrux.DB, o *options, w io.Writer) error {
	if err := need(o, 2, "FILE TABLE"); err != nil {
		return err
	}
	if err := db.Drop(o.pos[1]); err != nil {
		return err
	}
	fmt.Fprintf(w, "dropped %s, with its records and their links\n", o.pos[1])
	return nil
}

func check(db *hypercrux.DB, o *options, w io.Writer) error {
	if err := need(o, 1, "FILE"); err != nil {
		return err
	}
	rep, err := db.Check()
	if err != nil {
		return err
	}
	summary := strings.Join([]string{count(rep.Tables, "table"), count(rep.Records, "record"),
		count(rep.Links, "link"), count(rep.Vectors, "vector")}, ", ")
	if rep.OK() {
		fmt.Fprintln(w, "ok:", summary)
		return nil
	}
	fmt.Fprintln(w, "problems in", o.pos[0]+":")
	for _, p := range rep.Problems {
		fmt.Fprintln(w, "  -", p)
	}
	fmt.Fprintln(w, summary)
	return errProblems
}

// count writes a number with thousands separators and the noun after it.
func count(n int, noun string) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if n != 1 {
		noun += "s"
	}
	return s + " " + noun
}

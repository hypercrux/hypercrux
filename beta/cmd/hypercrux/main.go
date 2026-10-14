// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/hypercrux/hypercrux/beta/hypercrux"
)

// The verbs, their output, their messages and their exit codes are 0.x's,
// from cmd/hypercrux at the root of the repository, apart from what the Beta
// does differently (G5): no adopt, since a Beta database has no plain SQL
// tables; compact, which 0.x leaves to VACUUM; a version line without
// SQLite's; init making a database of an empty file, as any writer does;
// check printing damage that stops the database opening as a problem, and
// the sizes on a line of their own; and SQL in the Beta's subset, through
// the package's Query and Exec.

const usage = `hypercrux, the Beta: a native hybrid SQL, key-value, graph and vector database.

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
  hypercrux drop FILE TABLE                   delete a record table, its records and their links
  hypercrux check FILE                        confirm keys, records, links and vectors agree,
                                              and give the file's size and the memory it takes
  hypercrux compact FILE                      rewrite the file with only the live data
  hypercrux export FILE                       write every table, record and link as JSON lines
  hypercrux import FILE                       read an export from standard input into a new
                                              or empty file
  hypercrux version                           print the version

A key is table:id, such as docs:7. A VECTOR is a JSON array, such as
'[0.12, 0.8, 0.05]'. Options can go before or after the other arguments,
except with sql, whose options go before FILE: everything after FILE is the
statement and its arguments. Only init, put and import create a file.`

// The command, which doc.go's main runs. It's set here, in the one place
// that builds only on Linux, so the package has a main on every system.
func init() { command = start }

// start runs the command with the process's arguments and gives its exit
// code.
func start() int {
	out := bufio.NewWriter(os.Stdout)
	code := run(os.Args[1:], os.Stdin, out, os.Stderr)
	if err := out.Flush(); err != nil && code == 0 {
		fmt.Fprintln(os.Stderr, "hypercrux: writing the output:", err)
		code = 1
	}
	return code
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

// run runs the command with args, the arguments after its name, and
// returns its exit code: 0 when it worked, 1 when it failed, or check found
// problems, and 2 for a mistake in how it was called.
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
		return failed(1)
	default:
		msg := err.Error()
		if !strings.HasPrefix(msg, "hypercrux:") {
			msg = "hypercrux: " + msg
		}
		fmt.Fprintln(stderr, msg)
		return failed(1)
	}
}

// failed is the exit code for a verb that failed: code, unless a planted
// bug says otherwise.
func failed(code int) int {
	if plant == "cmd/failure-exits-zero" {
		return 0
	}
	return code
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
		fmt.Fprintf(stdout, "hypercrux %s, file format %d\n", hypercrux.Version, hypercrux.FormatVersion)
		return nil
	case "init":
		if err := need(o, 1, "FILE"); err != nil {
			return err
		}
		return initFile(o.pos[0], stdout)
	case "put":
		if len(o.pos) != 2 && len(o.pos) != 3 {
			return usageErr("expected FILE KEY [JSON]")
		}
		return put(o, stdin, stdout)
	case "import":
		if err := need(o, 1, "FILE"); err != nil {
			return err
		}
		return importFile(o.pos[0], stdin, stdout)
	}

	commands := map[string]func(*hypercrux.DB, *options, io.Writer) error{
		"get": get, "delete": del, "scan": scan, "sql": runSQL, "link": link, "unlink": unlink,
		"neighbours": neighbours, "neighbors": neighbours, "walk": walk, "nearest": nearest,
		"drop": drop, "check": check, "compact": compact, "export": exportFile,
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
		if cmd == "check" {
			return damaged(o.pos[0], err, stdout)
		}
		return err
	}
	defer db.Close()
	return f(db, o, stdout)
}

// openExisting opens a file that must already exist, so a typo in a file
// name doesn't create an empty database. Open refuses a file that isn't a
// Beta database, a 0.x one among them, with a message that says so.
func openExisting(path string) (*hypercrux.DB, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("%s: no such file (only init, put and import create one)", path)
	}
	if st.IsDir() {
		return nil, fmt.Errorf("%s is a directory", path)
	}
	return hypercrux.Open(path)
}

// initFile creates the database at path, or opens the one there. An empty
// file holds no database yet, and a write makes one of it, so init makes an
// Update that changes nothing, which takes the write lock and does that.
func initFile(path string, stdout io.Writer) error {
	db, err := hypercrux.Open(path)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.Update(func(*hypercrux.Tx) error { return nil }); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s is ready (HyperCrux file format %d)\n", path, hypercrux.FormatVersion)
	return nil
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
		if created && plant != "cmd/failed-put-leaves-file" {
			os.Remove(path) // don't leave an empty database behind
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
	if rep.OK() {
		fmt.Fprintln(w, "ok:", summary(rep))
		fmt.Fprintln(w, sizes(rep))
		return nil
	}
	fmt.Fprintln(w, "problems in", o.pos[0]+":")
	for _, p := range rep.Problems {
		fmt.Fprintln(w, "  -", p)
	}
	fmt.Fprintln(w, summary(rep))
	fmt.Fprintln(w, sizes(rep))
	return errProblems
}

// damaged is check's answer when the database doesn't open, with err: damage
// that stops it opening is a problem in the file, which check lists as it
// lists the ones Check finds, in Check's words, and any other error is the
// verb's.
func damaged(path string, err error, w io.Writer) error {
	var d *hypercrux.Damage
	if !errors.As(err, &d) {
		return err
	}
	// Check names damage as Damage's Error does, without the package's name
	// and the file's.
	words := (&hypercrux.Damage{Offset: d.Offset, Batch: d.Batch, Reason: d.Reason}).Error()
	fmt.Fprintln(w, "problems in", path+":")
	fmt.Fprintln(w, "  -", strings.TrimPrefix(words, "hypercrux: "))
	return errProblems
}

// compact rewrites the database's file with only the live data, and says
// how large the file was and is.
func compact(db *hypercrux.DB, o *options, w io.Writer) error {
	if err := need(o, 1, "FILE"); err != nil {
		return err
	}
	before, err := os.Stat(o.pos[0])
	if err != nil {
		return err
	}
	if plant != "cmd/compact-skipped" {
		if err := db.Compact(); err != nil {
			return err
		}
	}
	after, err := os.Stat(o.pos[0])
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "compacted %s from %s to %s\n", o.pos[0], count(int(before.Size()), "byte"), count(int(after.Size()), "byte"))
	return nil
}

func exportFile(db *hypercrux.DB, o *options, w io.Writer) error {
	if err := need(o, 1, "FILE"); err != nil {
		return err
	}
	return db.Export(w)
}

// importFile reads an export into a new file, or into an existing database
// without record tables, and checks the result.
func importFile(path string, stdin io.Reader, stdout io.Writer) error {
	_, statErr := os.Stat(path)
	created := os.IsNotExist(statErr)
	var db *hypercrux.DB
	var err error
	if created {
		db, err = hypercrux.Open(path)
	} else {
		db, err = openExisting(path)
	}
	if err != nil {
		return err
	}
	if err := db.Import(stdin); err != nil {
		db.Close()
		if created {
			os.Remove(path) // don't leave an empty database behind
		}
		return err
	}
	rep, err := db.Check()
	db.Close()
	if err != nil {
		return err
	}
	if !rep.OK() {
		fmt.Fprintln(stdout, "problems in", path+":")
		for _, p := range rep.Problems {
			fmt.Fprintln(stdout, "  -", p)
		}
		return errProblems
	}
	fmt.Fprintf(stdout, "imported %s into %s\n", summary(rep), path)
	return nil
}

// summary counts what a report found.
func summary(rep hypercrux.Report) string {
	return strings.Join([]string{count(rep.Tables, "table"), count(rep.Records, "record"),
		count(rep.Links, "link"), count(rep.Vectors, "vector")}, ", ")
}

// sizes gives the sizes a report found: the file's, and about how much
// memory the copy of the database takes in a process that opens it.
func sizes(rep hypercrux.Report) string {
	return fmt.Sprintf("size: %s in the file, about %s in memory", count(int(rep.FileBytes), "byte"), count(int(rep.MemoryBytes), "byte"))
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

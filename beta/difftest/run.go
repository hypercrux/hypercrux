// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package difftest

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	c "github.com/hypercrux/hypercrux/beta/conformance"
)

// errRollback is what an Update with Fail set returns from its function.
var errRollback = errors.New("difftest: rolled back on purpose")

// result is what one step gave back. Text is compared exactly; Hits, from
// Nearest, are compared within conformance.DistanceBound; Note holds the
// error messages, which differ between engines and are only reported.
type result struct {
	Text string
	Hits [][]c.Hit
	Note string
}

// kind names an error by what a caller can test: one of the two sentinel
// errors, the rollback, or anything else.
func kind(e c.Engine, err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, errRollback):
		return "rolled back"
	case errors.Is(err, e.ErrNotFound()):
		return "not found"
	case errors.Is(err, e.ErrInvalid()):
		return "invalid"
	}
	return "error"
}

func outcome(e c.Engine, err error, text string) result {
	r := result{Text: kind(e, err)}
	if err != nil {
		r.Note = err.Error()
	} else if text != "" {
		r.Text += " " + text
	}
	return r
}

// show writes a value with its type, and floats with their bits, so that
// -0 and 0, or 1 and 1.0, never look the same.
func show(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case int64:
		return "int " + strconv.FormatInt(x, 10)
	case float64:
		return fmt.Sprintf("real %v (%016x)", x, math.Float64bits(x))
	case string:
		return "text " + strconv.Quote(x)
	case []byte:
		return "bytes " + hex.EncodeToString(x)
	case bool:
		return "bool " + strconv.FormatBool(x)
	case c.Vector:
		parts := make([]string, len(x))
		for i, f := range x {
			parts[i] = fmt.Sprintf("%v (%08x)", f, math.Float32bits(f))
		}
		return "vector [" + strings.Join(parts, ", ") + "]"
	case time.Time:
		return "time " + x.Format(time.RFC3339Nano)
	}
	return fmt.Sprintf("%T %v", v, v)
}

func showFields(f c.Fields) string {
	names := make([]string, 0, len(f))
	for n := range f {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, len(names))
	for i, n := range names {
		parts[i] = n + ": " + show(f[n])
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func showRows(rows *sql.Rows) (string, error) {
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return "", err
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	lines := []string{strings.Join(cols, ", ")}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return "", err
		}
		parts := make([]string, len(vals))
		for i, v := range vals {
			parts[i] = show(v)
		}
		lines = append(lines, strings.Join(parts, ", "))
	}
	return strings.Join(lines, "; "), rows.Err()
}

func showLinks(links []c.Link) string {
	parts := make([]string, len(links))
	for i, l := range links {
		parts[i] = fmt.Sprintf("%q -%q-> %q", l.From, l.Type, l.To)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func args(vs []Value) []any {
	out := make([]any, len(vs))
	for i, v := range vs {
		out[i] = v.V
	}
	return out
}

func dir(d int) c.Direction {
	switch d {
	case 1:
		return c.In
	case 2:
		return c.Both
	}
	return c.Out
}

// do runs one step on h. update is the database's Update, for a step that
// is an Update; inside one it's nil.
func do(e c.Engine, h c.Handle, update func(func(c.Handle) error) error, op Op) result {
	switch op.Kind {
	case "put":
		f := c.Fields{}
		for _, fl := range op.Fields {
			f[fl.Name] = fl.Value.V
		}
		return outcome(e, h.Put(op.Key, f), "")
	case "get":
		f, err := h.Get(op.Key)
		return outcome(e, err, showFields(f))
	case "delete":
		return outcome(e, h.Delete(op.Key), "")
	case "scan":
		recs, err := h.Scan(op.Key, op.After, op.N)
		parts := make([]string, len(recs))
		for i, r := range recs {
			parts[i] = strconv.Quote(r.Key) + " " + showFields(r.Fields)
		}
		return outcome(e, err, "["+strings.Join(parts, "; ")+"]")
	case "link":
		return outcome(e, h.Link(op.Key, op.Type, op.To), "")
	case "unlink":
		return outcome(e, h.Unlink(op.Key, op.Type, op.To), "")
	case "neighbours":
		links, err := h.Neighbours(op.Key, dir(op.Dir), op.Type)
		return outcome(e, err, showLinks(links))
	case "walk":
		steps, err := h.Walk(op.Key, dir(op.Dir), op.Type, op.N)
		parts := make([]string, len(steps))
		for i, s := range steps {
			parts[i] = fmt.Sprintf("%q at %d", s.Key, s.Depth)
		}
		return outcome(e, err, "["+strings.Join(parts, ", ")+"]")
	case "nearest":
		var q c.Vector
		if op.Vec != nil {
			q, _ = op.Vec.V.(c.Vector)
		}
		hits, err := h.Nearest(op.Table, q, op.N, op.Where, args(op.Args)...)
		r := outcome(e, err, "")
		if err == nil {
			r.Hits = [][]c.Hit{hits}
		}
		return r
	case "drop":
		return outcome(e, h.Drop(op.Table), "")
	case "sql":
		if strings.HasPrefix(op.SQL, "SELECT") {
			rows, err := h.Query(op.SQL, args(op.Args)...)
			if err != nil {
				return outcome(e, err, "")
			}
			text, err := showRows(rows)
			return outcome(e, err, text)
		}
		res, err := h.Exec(op.SQL, args(op.Args)...)
		if err != nil {
			return outcome(e, err, "")
		}
		n, err := res.RowsAffected()
		return outcome(e, err, fmt.Sprintf("%d rows", n))
	case "update":
		if update == nil {
			return result{Text: "an Update inside an Update"}
		}
		var inner []result
		err := update(func(tx c.Handle) error {
			for _, o := range op.Ops {
				inner = append(inner, do(e, tx, nil, o))
			}
			if op.Fail {
				return errRollback
			}
			return nil
		})
		r := outcome(e, err, "")
		texts, notes := []string{}, []string{}
		for _, in := range inner {
			texts = append(texts, in.Text)
			notes = append(notes, in.Note)
			r.Hits = append(r.Hits, in.Hits...)
		}
		r.Text = "[" + strings.Join(texts, " | ") + "] " + r.Text
		r.Note = strings.Join(append(notes, r.Note), " | ")
		return r
	}
	return result{Text: "unknown step " + op.Kind}
}

func compare(a, b result) error {
	if a.Text != b.Text {
		return errors.New("the answers differ")
	}
	if len(a.Hits) != len(b.Hits) {
		return fmt.Errorf("%d searches against %d", len(a.Hits), len(b.Hits))
	}
	for i := range a.Hits {
		if err := c.CompareHits(b.Hits[i], a.Hits[i]); err != nil {
			return fmt.Errorf("search %d: %v", i+1, err)
		}
	}
	return nil
}

// state describes everything a database holds, as seen through the API:
// every record of the tables a sequence can touch, with its vector, every
// link out of it, and what Check counts.
func state(e c.Engine, db c.DB) (string, error) {
	var lines []string
	for _, t := range tables {
		recs, err := db.Scan(t+":", "", 0)
		if err != nil {
			return "", fmt.Errorf("scanning %s: %w", t, err)
		}
		for _, r := range recs {
			f, err := db.Get(r.Key)
			if err != nil {
				return "", fmt.Errorf("getting %s: %w", r.Key, err)
			}
			links, err := db.Neighbours(r.Key, c.Out, "")
			if err != nil {
				return "", fmt.Errorf("neighbours of %s: %w", r.Key, err)
			}
			lines = append(lines, fmt.Sprintf("%q %s links %s", r.Key, showFields(f), showLinks(links)))
		}
	}
	rep, err := db.Check()
	if err != nil {
		return "", fmt.Errorf("check: %w", err)
	}
	lines = append(lines, fmt.Sprintf("check: %d tables, %d records, %d links, %d vectors, %d problems",
		rep.Tables, rep.Records, rep.Links, rep.Vectors, len(rep.Problems)))
	return strings.Join(lines, "\n"), nil
}

// Mismatch is the first place two engines disagreed.
type Mismatch struct {
	Step   int    // the step's place in the sequence, or its length for the final state
	What   string // the step, as JSON
	A, B   string // what each engine gave
	Detail string
}

func (m *Mismatch) Error() string {
	return fmt.Sprintf("step %d, %s: %s\n  first engine:  %s\n  second engine: %s", m.Step, m.What, m.Detail, m.A, m.B)
}

func opJSON(op Op) string {
	b, err := json.Marshal(op)
	if err != nil {
		return fmt.Sprintf("%+v", op)
	}
	return string(b)
}

func open(e c.Engine, dir string) (c.DB, error) {
	return e.Open(filepath.Join(dir, "db"))
}

// Replay runs a sequence on a new database for each engine, in a new folder
// under dir, and returns the first mismatch, or nil when they agree from
// start to end.
func Replay(a, b c.Engine, ops []Op, dir string) (*Mismatch, error) {
	da, err := os.MkdirTemp(dir, "a")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(da)
	db, err := os.MkdirTemp(dir, "b")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(db)
	x, err := open(a, da)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", a.Name(), err)
	}
	defer x.Close()
	y, err := open(b, db)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", b.Name(), err)
	}
	defer y.Close()
	for i, op := range ops {
		ra, rb := do(a, x, x.Update, op), do(b, y, y.Update, op)
		if err := compare(ra, rb); err != nil {
			return &Mismatch{Step: i, What: opJSON(op), A: ra.Text + note(ra), B: rb.Text + note(rb), Detail: err.Error()}, nil
		}
	}
	sa, errA := state(a, x)
	sb, errB := state(b, y)
	if errA != nil || errB != nil || sa != sb {
		m := &Mismatch{Step: len(ops), What: "the state after the last step", Detail: "the databases differ"}
		la, lb := strings.Split(sa, "\n"), strings.Split(sb, "\n")
		for i := 0; i < len(la) || i < len(lb); i++ {
			if i >= len(la) || i >= len(lb) || la[i] != lb[i] {
				m.A, m.B = line(la, i, errA), line(lb, i, errB)
				break
			}
		}
		return m, nil
	}
	return nil, nil
}

func note(r result) string {
	if strings.Trim(r.Note, " |") == "" {
		return ""
	}
	return " (" + r.Note + ")"
}

func line(lines []string, i int, err error) string {
	switch {
	case err != nil:
		return "error: " + err.Error()
	case i < len(lines):
		return lines[i]
	}
	return "(nothing)"
}

// Options say how many sequences to run, and where.
type Options struct {
	Sequences int      // how many
	MaxLength int      // the longest, in steps; lengths vary from 5 up to this
	Seed      uint64   // the first sequence's seed; each after it adds one
	Dir       string   // where the databases go, a folder per sequence
	Leave     []string // kinds of step to leave out, from Kinds
}

// Failure is a sequence two engines disagreed on, shrunk as far as it would
// go while they still disagree. It's saved as JSON.
type Failure struct {
	Engines  [2]string `json:"engines"`
	Seed     uint64    `json:"seed"`
	Mismatch string    `json:"mismatch"`
	Ops      []Op      `json:"ops"`
}

func (f *Failure) String() string {
	b, _ := json.MarshalIndent(f.Ops, "", "  ")
	return fmt.Sprintf("%s and %s disagree, from seed %d, shrunk to %d steps:\n%s\nsteps: %s",
		f.Engines[0], f.Engines[1], f.Seed, len(f.Ops), f.Mismatch, b)
}

// Find runs random sequences on two engines until they disagree, and
// returns that sequence shrunk, or nil when they agree on every one.
func Find(a, b c.Engine, o Options) (*Failure, error) {
	if o.MaxLength < 5 {
		o.MaxLength = 5
	}
	for i := 0; i < o.Sequences; i++ {
		seed := o.Seed + uint64(i)
		n := 5 + rand.New(rand.NewPCG(seed, 1)).IntN(o.MaxLength-4)
		ops := Generate(seed, n, o.Leave...)
		m, err := Replay(a, b, ops, o.Dir)
		if err != nil {
			return nil, err
		}
		if m == nil {
			continue
		}
		small, m := shrink(a, b, ops, o.Dir, m)
		return &Failure{Engines: [2]string{a.Name(), b.Name()}, Seed: seed, Mismatch: m.Error(), Ops: small}, nil
	}
	return nil, nil
}

// shrink drops steps while the engines still disagree: everything after
// the first mismatch, then halves, quarters and so on down to single steps,
// then single steps inside each Update, then single fields of each Put.
func shrink(a, b c.Engine, ops []Op, dir string, m *Mismatch) ([]Op, *Mismatch) {
	fails := func(cand []Op) *Mismatch {
		mm, err := Replay(a, b, cand, dir)
		if err != nil {
			return nil
		}
		return mm
	}
	if m.Step < len(ops)-1 {
		if mm := fails(ops[:m.Step+1]); mm != nil {
			ops, m = ops[:m.Step+1], mm
		}
	}
	for size := len(ops) / 2; size >= 1; size /= 2 {
		for i := 0; i+size <= len(ops); {
			cand := append(append([]Op{}, ops[:i]...), ops[i+size:]...)
			if mm := fails(cand); mm != nil {
				ops, m = cand, mm
				continue
			}
			i += size
		}
	}
	for i := range ops {
		for j := 0; j < len(ops[i].Ops); {
			cand := append([]Op{}, ops...)
			cand[i].Ops = append(append([]Op{}, ops[i].Ops[:j]...), ops[i].Ops[j+1:]...)
			if mm := fails(cand); mm != nil {
				ops, m = cand, mm
				continue
			}
			j++
		}
	}
	for i := range ops {
		for j := 0; j < len(ops[i].Fields); {
			cand := append([]Op{}, ops...)
			cand[i].Fields = append(append([]Field{}, ops[i].Fields[:j]...), ops[i].Fields[j+1:]...)
			if mm := fails(cand); mm != nil {
				ops, m = cand, mm
				continue
			}
			j++
		}
		for k := range ops[i].Ops {
			for j := 0; j < len(ops[i].Ops[k].Fields); {
				cand := append([]Op{}, ops...)
				cand[i].Ops = append([]Op{}, ops[i].Ops...)
				cand[i].Ops[k].Fields = append(append([]Field{}, ops[i].Ops[k].Fields[:j]...), ops[i].Ops[k].Fields[j+1:]...)
				if mm := fails(cand); mm != nil {
					ops, m = cand, mm
					continue
				}
				j++
			}
		}
	}
	return ops, m
}

// Save writes a failure into dir as JSON, under a name taken from its
// steps, and returns the file's path.
func Save(dir string, f *Failure) (string, error) {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return "", err
	}
	steps, _ := json.Marshal(f.Ops)
	sum := sha256.Sum256(steps)
	path := filepath.Join(dir, fmt.Sprintf("failing-%x.json", sum[:6]))
	return path, os.WriteFile(path, append(b, '\n'), 0o644)
}

// Load reads a failure Save wrote.
func Load(path string) (*Failure, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f Failure
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &f, nil
}

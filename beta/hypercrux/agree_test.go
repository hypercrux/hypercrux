// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	zx "github.com/hypercrux/hypercrux"
	hc "github.com/hypercrux/hypercrux/beta/hypercrux"
)

// TestTheBetaAgrees0x runs the same random puts, gets, deletes and Updates
// on 0.x and on the Beta, through their Go APIs, and now and then closes
// the Beta and opens its file again. Every call must give the same answer
// on both: the same error, with the same message and of the same kind, or
// the same fields, with every number's bits. After each reopen, and at the
// end, every key the workload can name must read the same on both, and
// Check must give the same counts. Only the message for one field in two
// spellings may differ, since 0.x reports whichever of the two its map
// gave first.
//
// An Update holds one to five calls through its transaction, and commits,
// returns an error or panics. A call in it that fails doesn't end it, as
// with 0.x. 0.x is the judge throughout: of the rules and their order of
// errors, of the conversions from Go values, and of what a reopen must
// read back.
func TestTheBetaAgrees0x(t *testing.T) {
	steps := 1500
	if testing.Short() {
		steps = 300
	}
	for seed := uint64(1); seed <= 2; seed++ {
		t.Run(fmt.Sprint("seed ", seed), func(t *testing.T) { agree0x(t, seed, steps) })
	}
}

func agree0x(t *testing.T, seed uint64, steps int) {
	dir := t.TempDir()
	z, err := zx.Open(filepath.Join(dir, "zero.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer z.Close()
	path := filepath.Join(dir, "beta.hcx")
	b, err := hc.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { b.Close() }()

	g := &workload{r: rand.New(rand.NewPCG(seed, 0x61)), sizes: map[string]int{}}
	outcomes := map[string]int{}
	note := func(what string, err error) { outcomes[what+" "+kindOf(err)]++ }
	for step := 0; step < steps; step++ {
		switch w := g.r.IntN(100); {
		case w < 40:
			key, f := g.put()
			zErr, bErr := z.Put(key, f.zero()), b.Put(key, f.beta())
			sameErr(t, step, fmt.Sprintf("put %q %v", key, f), bErr, zErr)
			g.learn(z, key, zErr)
			note("put", zErr)
		case w < 60:
			key := g.key()
			zf, zErr := z.Get(key)
			bf, bErr := b.Get(key)
			sameErr(t, step, "get "+key, bErr, zErr)
			sameFields(t, step, "get "+key, bf, zf)
			note("get", zErr)
		case w < 70:
			key := g.key()
			zErr, bErr := z.Delete(key), b.Delete(key)
			sameErr(t, step, "delete "+key, bErr, zErr)
			note("delete", zErr)
		case w < 95:
			outcome := g.r.IntN(4) // 0: an error, 1: a panic, else a commit
			calls := g.update()
			var zSaid, bSaid []string
			zErr := runUpdate(func(fn func(h handle) error) error {
				return z.Update(func(tx *zx.Tx) error { return fn(zeroTx{tx}) })
			}, calls, outcome, &zSaid)
			bErr := runUpdate(func(fn func(h handle) error) error {
				return b.Update(func(tx *hc.Tx) error { return fn(betaTx{tx}) })
			}, calls, outcome, &bSaid)
			desc := fmt.Sprintf("update %v, ending %d", calls, outcome)
			sameErr(t, step, desc, bErr, zErr)
			for i := range zSaid {
				if bSaid[i] != zSaid[i] {
					t.Fatalf("step %d: %s: call %d gives on the Beta\n  %s\nand on 0.x\n  %s", step, desc, i, bSaid[i], zSaid[i])
				}
			}
			if zErr == nil {
				for _, c := range calls {
					if c.op == "put" {
						g.learn(z, c.key, nil)
					}
				}
			}
			note("update", zErr)
		default:
			if err := b.Close(); err != nil {
				t.Fatal(err)
			}
			if b, err = hc.Open(path); err != nil {
				t.Fatalf("step %d: reopening: %v", step, err)
			}
			g.everything(t, step, z, b)
			note("reopen", nil)
		}
	}
	b.Close()
	if b, err = hc.Open(path); err != nil {
		t.Fatal(err)
	}
	g.everything(t, steps, z, b)
	for _, o := range []string{"put ok", "put invalid", "get ok", "get invalid", "get not found", "delete ok",
		"delete not found", "update ok", "update error", "reopen ok"} {
		if outcomes[o] < steps/300 {
			t.Errorf("%q came up %d times in %d steps: %v", o, outcomes[o], steps, outcomes)
		}
	}
	if testing.Verbose() {
		t.Logf("outcomes: %v", outcomes)
	}
}

// workload makes the random calls, with a rough idea of each table's vector
// size, learnt from 0.x, so that most vectors fit.
type workload struct {
	r     *rand.Rand
	sizes map[string]int
}

var (
	tables   = []string{"docs", "people", "t_1"}
	ids      = []string{"1", "2", "3", "4", "é", "a b", "x/y"}
	names    = []string{"title", "Title", "TITLE", "n", "N", "score", "_x", "Zeta", "zeta", "a1", "vec", "Vec", "VEC"}
	badNames = []string{"key", "Rowid", "1st", "has space", "oid"}
	badKeys  = []string{"nocolon", "docs:", "Docs:1", "docs:\x00", "hc_x:1", ":1", "docs:\xff"}
)

type status string
type blob []byte

func (g *workload) pick(list []string) string { return list[g.r.IntN(len(list))] }

func (g *workload) key() string {
	if g.r.IntN(20) == 0 {
		return g.pick(badKeys)
	}
	return g.pick(tables) + ":" + g.pick(ids)
}

// value returns a Go value for a field other than vec: of every type Put
// takes, awkward ones among them, and now and then one it refuses.
func (g *workload) value() any {
	when := time.Date(2026, 10, 8, 9, 30, 0, 123456789, time.FixedZone("IDT", 3*3600))
	n := 7
	var nilInt *int
	values := []any{
		"Q3 plan", "", "é ✓ 😀", "a\x00b", 12, -1, int64(math.MaxInt64), int64(math.MinInt64), int8(-8), uint32(math.MaxUint32),
		uint64(math.MaxInt64), 0.75, math.Copysign(0, -1), math.SmallestNonzeroFloat64, math.MaxFloat64, float32(0.1),
		true, false, []byte{0, 1, 255}, []byte{}, nil, nilInt, &n, []any{"a", 1.5}, map[string]any{"k": 1},
		map[int]string{1: "a"}, struct{ X, Y int }{1, 2}, []string{"a"}, when, status("open"), blob{1, 2},
		json.Number("42"), json.Number("4.5"),
		// Refused by 0.x's rules.
		"\xff", math.NaN(), math.Inf(-1), uint64(math.MaxUint64), []float32{1}, make(chan int),
		map[string]any{"f": func() {}},
	}
	if g.r.IntN(3) == 0 {
		return values[g.r.IntN(len(values))]
	}
	return values[g.r.IntN(len(values)-7)]
}

// vector returns a value for vec: mostly a vector of the table's size, in
// any of the forms Put takes, and now and then the wrong size, all zeros,
// holding NaN, or something that isn't a vector at all.
func (g *workload) vector(tbl string) any {
	n := g.sizes[tbl]
	if n == 0 || g.r.IntN(12) == 0 {
		n = 1 + g.r.IntN(4)
	}
	v := make([]float32, n)
	for i := range v {
		v[i] = float32(g.r.NormFloat64())
	}
	if g.r.IntN(8) == 0 {
		v[g.r.IntN(n)] = []float32{float32(math.Copysign(0, -1)), math.SmallestNonzeroFloat32, math.MaxFloat32}[g.r.IntN(3)]
	}
	switch g.r.IntN(16) {
	case 0:
		return nil
	case 1:
		return make([]float32, n)
	case 2:
		v[0] = float32(math.NaN())
	case 3:
		f := make([]float64, n)
		for i := range f {
			f[i] = float64(v[i])
		}
		return f
	case 4:
		b, _ := json.Marshal(v)
		return string(b)
	case 5:
		a := make([]any, n)
		for i := range a {
			a[i] = float64(v[i])
		}
		return a
	case 6:
		return 5
	case 7:
		return vecPtr(v)
	case 8:
		return "not a vector"
	case 9:
		return v
	}
	return vec(v)
}

// vec stands for a vector of the package's own type, a Vector on each
// side, and vecPtr for a pointer to one.
type vec []float32
type vecPtr []float32

// goFields are a put's fields, with vec and vecPtr standing in for each
// package's Vector.
type goFields map[string]any

func (f goFields) zero() zx.Fields {
	if f == nil {
		return nil
	}
	out := zx.Fields{}
	for k, v := range f {
		switch x := v.(type) {
		case vec:
			v = zx.Vector(x)
		case vecPtr:
			p := zx.Vector(x)
			v = &p
		}
		out[k] = v
	}
	return out
}

func (f goFields) beta() hc.Fields {
	if f == nil {
		return nil
	}
	out := hc.Fields{}
	for k, v := range f {
		switch x := v.(type) {
		case vec:
			v = hc.Vector(x)
		case vecPtr:
			p := hc.Vector(x)
			v = &p
		}
		out[k] = v
	}
	return out
}

// put returns a key and fields to put there: zero to four fields, with
// names in mixed case. A put holds at most one name that breaks the rules,
// or else one field in two spellings, since 0.x checks the names in its
// map's order and reports whichever problem it meets first.
func (g *workload) put() (string, goFields) {
	key := g.key()
	tbl, _, _ := strings.Cut(key, ":")
	if g.r.IntN(15) == 0 {
		return key, nil
	}
	f := goFields{}
	bad, clash := false, false
	for n := g.r.IntN(5); n > 0; n-- {
		name := g.pick(names)
		if !bad && !clash && g.r.IntN(30) == 0 {
			name, bad = g.pick(badNames), true
		}
		for have := range f {
			if strings.EqualFold(have, name) && have != name {
				if bad {
					name = have
				}
				clash = clash || !bad
			}
		}
		if strings.EqualFold(name, "vec") {
			f[name] = g.vector(tbl)
		} else {
			f[name] = g.value()
		}
	}
	return key, f
}

// learn notes a table's vector size from 0.x once a put of key has
// worked, so later vectors mostly fit.
func (g *workload) learn(z *zx.DB, key string, err error) {
	if err != nil {
		return
	}
	tbl, _, _ := strings.Cut(key, ":")
	var dims *int64
	if z.QueryRow(`SELECT dims FROM hc_tables WHERE name = ?`, tbl).Scan(&dims) == nil && dims != nil {
		g.sizes[tbl] = int(*dims)
	}
}

// call is one call inside an Update.
type call struct {
	op  string
	key string
	f   goFields
}

func (c call) String() string {
	if c.op == "put" {
		return fmt.Sprintf("put %q %v", c.key, c.f)
	}
	return c.op + " " + c.key
}

func (g *workload) update() []call {
	calls := make([]call, 1+g.r.IntN(5))
	for i := range calls {
		switch w := g.r.IntN(10); {
		case w < 6:
			key, f := g.put()
			calls[i] = call{"put", key, f}
		case w < 8:
			calls[i] = call{"get", g.key(), nil}
		default:
			calls[i] = call{"delete", g.key(), nil}
		}
	}
	return calls
}

// handle is what both packages' transactions do, with each one's fields.
type handle interface {
	get(key string) (map[string]any, error)
	put(key string, f goFields) error
	delete(key string) error
}

type zeroTx struct{ tx *zx.Tx }

func (h zeroTx) get(key string) (map[string]any, error) {
	f, err := h.tx.Get(key)
	return f, err
}
func (h zeroTx) put(key string, f goFields) error { return h.tx.Put(key, f.zero()) }
func (h zeroTx) delete(key string) error          { return h.tx.Delete(key) }

type betaTx struct{ tx *hc.Tx }

func (h betaTx) get(key string) (map[string]any, error) {
	f, err := h.tx.Get(key)
	return f, err
}
func (h betaTx) put(key string, f goFields) error { return h.tx.Put(key, f.beta()) }
func (h betaTx) delete(key string) error          { return h.tx.Delete(key) }

var errRollback = errors.New("rolled back on purpose")

// runUpdate makes calls inside one Update through update, noting what each
// gave in said, and ends the function by outcome: 0 returns an error, 1
// panics, and anything else commits.
func runUpdate(update func(fn func(h handle) error) error, calls []call, outcome int, said *[]string) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("panicked: %v", p)
		}
	}()
	return update(func(h handle) error {
		for _, c := range calls {
			var e error
			var f map[string]any
			switch c.op {
			case "put":
				e = h.put(c.key, c.f)
			case "get":
				f, e = h.get(c.key)
			case "delete":
				e = h.delete(c.key)
			}
			*said = append(*said, describeErr(e)+" "+show(f))
		}
		switch outcome {
		case 0:
			return errRollback
		case 1:
			panic("on purpose")
		}
		return nil
	})
}

// everything compares every key the workload can name on both databases,
// and Check's counts.
func (g *workload) everything(t *testing.T, step int, z *zx.DB, b *hc.DB) {
	t.Helper()
	for _, tbl := range tables {
		for _, id := range ids {
			key := tbl + ":" + id
			zf, zErr := z.Get(key)
			bf, bErr := b.Get(key)
			sameErr(t, step, "after a reopen, get "+key, bErr, zErr)
			sameFields(t, step, "after a reopen, get "+key, bf, zf)
		}
	}
	zr, zErr := z.Check()
	br, bErr := b.Check()
	if zErr != nil || bErr != nil || !zr.OK() || !br.OK() ||
		br.Tables != zr.Tables || br.Records != zr.Records || br.Links != zr.Links || br.Vectors != zr.Vectors {
		t.Fatalf("step %d: after a reopen, Check gives %+v, %v on the Beta, and %+v, %v on 0.x", step, br, bErr, zr, zErr)
	}
}

// kindOf names an error by what a caller can test.
func kindOf(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, hc.ErrNotFound) || errors.Is(err, zx.ErrNotFound):
		return "not found"
	case errors.Is(err, hc.ErrInvalid) || errors.Is(err, zx.ErrInvalid):
		return "invalid"
	}
	return "error"
}

// describeErr shows an error with its kind, and with its message, apart
// from 0.x's message for one field in two spellings, whose name comes from
// its map's order.
func describeErr(err error) string {
	if err == nil {
		return "ok"
	}
	if strings.Contains(err.Error(), "differ only in case") {
		return kindOf(err) + ": a field in two spellings"
	}
	return kindOf(err) + ": " + err.Error()
}

func sameErr(t *testing.T, step int, what string, beta, zero error) {
	t.Helper()
	if describeErr(beta) != describeErr(zero) {
		t.Fatalf("step %d: %s: the Beta gives %v, and 0.x %v", step, what, beta, zero)
	}
}

func sameFields[B ~map[string]any, Z ~map[string]any](t *testing.T, step int, what string, beta B, zero Z) {
	t.Helper()
	if b, z := show(beta), show(zero); b != z {
		t.Fatalf("step %d: %s: the Beta gives\n  %s\nand 0.x\n  %s", step, what, b, z)
	}
}

// show writes fields with their types, and numbers with their bits, so
// that -0 and 0, or a whole number and a real, never look the same. Each
// package's Vector shows the same way, as does a nil map and an empty one,
// which 0.x never gives for a record that exists.
func show[F ~map[string]any](f F) string {
	if f == nil {
		return "nil"
	}
	names := slices.Sorted(func(yield func(string) bool) {
		for k := range f {
			if !yield(k) {
				return
			}
		}
	})
	parts := make([]string, len(names))
	for i, n := range names {
		var s string
		switch x := f[n].(type) {
		case float64:
			s = fmt.Sprintf("float64 %016x", math.Float64bits(x))
		case int64:
			s = fmt.Sprintf("int64 %d", x)
		case string:
			s = fmt.Sprintf("string %q", x)
		case []byte:
			s = fmt.Sprintf("[]byte %x (nil %v)", x, x == nil)
		case hc.Vector:
			s = "Vector " + bitsOf(x)
		case zx.Vector:
			s = "Vector " + bitsOf(x)
		default:
			s = fmt.Sprintf("%T %#v", x, x)
		}
		parts[i] = n + ": " + s
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

func bitsOf[V ~[]float32](v V) string {
	parts := make([]string, len(v))
	for i, x := range v {
		parts[i] = fmt.Sprintf("%08x", math.Float32bits(x))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

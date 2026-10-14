// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package store

import (
	"errors"
	"math"
	"slices"
	"syscall"
	"testing"
	"unsafe"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
	"github.com/hypercrux/hypercrux/beta/internal/format"
	"github.com/hypercrux/hypercrux/beta/internal/rules"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// L1: text and bytes stop at SQLite's 1,000,000,000 bytes, rules.MaxLength,
// as in 0.x. A value one byte longer is refused by Put, a transaction's Put
// and Apply, and a batch that holds one is damage, as a batch that breaks
// any other rule is. The values here are a gigabyte long without taking a
// gigabyte: huge maps them from the kernel's page of zeros, which costs
// nothing until something writes to it, and the store refuses them before
// it copies anything. rules.TestLength holds the limit itself to its edge,
// on lengths alone.

// huge returns n bytes, head followed by zero bytes and then tail, in memory
// the kernel maps from its page of zeros, which costs nothing to read. Only
// the pages head and tail fall on take memory, and the mapping is read only,
// so a write to it stops the test. A store that copied the value would take
// n bytes of memory, so the tests below show nothing is copied before the
// refusal by keeping their memory small.
func huge(t testing.TB, n int, head, tail string) string {
	t.Helper()
	prot := syscall.PROT_READ
	if head != "" || tail != "" {
		prot |= syscall.PROT_WRITE
	}
	b, err := syscall.Mmap(-1, 0, n, prot, syscall.MAP_PRIVATE|syscall.MAP_ANON|syscall.MAP_NORESERVE)
	if err != nil {
		t.Fatalf("mapping %d bytes: %v", n, err)
	}
	t.Cleanup(func() { syscall.Munmap(b) })
	syscall.Madvise(b, 14) // MADV_HUGEPAGE: the zeros in pages of 2 MB, read with fewer faults
	if prot&syscall.PROT_WRITE != 0 {
		copy(b, head)
		copy(b[n-len(tail):], tail)
		if err := syscall.Mprotect(b, syscall.PROT_READ); err != nil {
			t.Fatal(err)
		}
	}
	return unsafe.String(&b[0], n)
}

// tooBig checks that err is SQLite's, of kind "error", as 0.x gives it. It
// stops the test at once otherwise: a store that took a value a gigabyte
// long has copied it, and the checks after would copy it again.
func tooBig(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil || err.Error() != "string or blob too big" || errors.Is(err, errs.ErrInvalid) || errors.Is(err, errs.ErrNotFound) {
		t.Fatalf("%s gave %v, want SQLite's error, of kind error", what, err)
	}
}

func TestTheLimitOnTextAndBytes(t *testing.T) {
	over := huge(t, rules.MaxLength+1, "", "")
	at := huge(t, rules.MaxLength, "", "")
	field := func(name string, v value.Value) format.Field { return format.Field{Name: name, Value: v} }

	// The check itself, on values at its edge, which it reads the length of
	// alone. Neither is copied.
	ok(t, lengths([]format.Field{field("b", value.Bytes(at)), field("t", value.Text(at)), field("n", value.Int(1))}))
	tooBig(t, "lengths of bytes one byte over", lengths([]format.Field{field("b", value.Bytes(over))}))
	tooBig(t, "lengths of text one byte over", lengths([]format.Field{field("a", value.Int(1)), field("t", value.Text(over))}))

	s := New()
	_, err := s.Put(nil, "docs:1", []format.Field{field("title", value.Text("a"))})
	ok(t, err)
	before := dump(s)

	// Put, into a table that's there and into one that isn't, a record
	// that's there and one that isn't, with a field the table has and one it
	// hasn't: nothing changes, the table's fields included, and the change
	// list comes back as it went in.
	dst := []format.Change{{Op: format.Delete, Key: "docs:9"}}
	for _, c := range []struct {
		key    string
		fields []format.Field
	}{
		{"docs:1", []format.Field{field("title", value.Text(over))}},
		{"docs:1", []format.Field{field("body", value.Bytes(over)), field("n", value.Int(2))}},
		{"docs:2", []format.Field{field("a", value.Text("x")), field("z", value.Text(over))}},
		{"notes:1", []format.Field{field("b", value.Bytes(over))}},
	} {
		got, err := s.Put(dst, c.key, c.fields)
		tooBig(t, "a Put of "+c.key, err)
		if len(got) != 1 || got[0].Key != "docs:9" {
			t.Errorf("a Put of %s that failed gave back the change list %v", c.key, got)
		}
	}
	if after := dump(s); after != before {
		t.Errorf("Puts that failed changed the store:\n%s\nwas\n%s", after, before)
	}

	// 0.x's order: each of Put's other checks comes first, as 0.x makes them
	// before it binds the values, wherever the long value sorts.
	_, err = s.Put(nil, "vecs:1", []format.Field{field("vec", value.Vector([]float32{1, 2}))})
	ok(t, err)
	before = dump(s)
	invalidUTF8 := huge(t, rules.MaxLength+1, "\xff", "")
	for _, c := range []struct {
		what   string
		key    string
		fields []format.Field
		want   string
	}{
		{"a key that breaks the rules", "Docs:1", []format.Field{field("t", value.Text(over))},
			`key "Docs:1": hypercrux: invalid: table name "Docs": use lower-case letters, digits and underscores, starting with a letter`},
		{"an infinite real after it", "docs:1", []format.Field{field("a", value.Text(over)), field("b", value.Real(math.Inf(1)))},
			"hypercrux: invalid: field b is +Inf"},
		{"an infinite real before it", "docs:1", []format.Field{field("a", value.Real(math.Inf(-1))), field("b", value.Bytes(over))},
			"hypercrux: invalid: field a is -Inf"},
		{"text that isn't UTF-8", "docs:1", []format.Field{field("t", value.Text(invalidUTF8))},
			"hypercrux: invalid: field t isn't valid UTF-8; store bytes as []byte"},
		{"a vector of another size", "vecs:2", []format.Field{field("a", value.Bytes(over)), field("vec", value.Vector([]float32{1, 2, 3}))},
			"hypercrux: invalid: table vecs holds vectors of 2 values, and vecs:2 has 3"},
	} {
		_, err := s.Put(nil, c.key, c.fields)
		if err == nil {
			t.Fatalf("a Put of a long value and %s worked, want %s", c.what, c.want)
		}
		if err.Error() != c.want {
			t.Errorf("a Put of a long value and %s gave %v, want %s", c.what, err, c.want)
		}
	}
	wide := make([]format.Field, rules.MaxFields+1)
	for i := range wide {
		wide[i] = field("f"+string(rune('a'+i%26))+string(rune('a'+i/26%26))+string(rune('a'+i/676)), value.Null())
	}
	wide[0].Value = value.Bytes(over)
	if _, err := s.Put(nil, "wide:1", wide); !errors.Is(err, errs.ErrInvalid) {
		t.Fatalf("a Put of a long value into a table past %d fields gave %v, want the fields' error", rules.MaxFields, err)
	}
	if after := dump(s); after != before {
		t.Errorf("Puts that failed changed the store:\n%s\nwas\n%s", after, before)
	}

	// A transaction's Put and Apply fail, and the transaction carries on
	// with its change list as it was.
	tx := begin(t, s)
	ok(t, tx.Put("docs:3", []format.Field{field("n", value.Int(3))}))
	tooBig(t, "a transaction's Put", tx.Put("docs:1", []format.Field{field("t", value.Text(over))}))
	tooBig(t, "a transaction's Apply", tx.Apply(format.Change{Op: format.Put, Key: "docs:4", Fields: []format.Field{field("b", value.Bytes(over))}}))
	ok(t, tx.Put("docs:5", []format.Field{field("n", value.Int(5))}))
	if n := len(tx.changes); n != 2 {
		t.Errorf("the transaction's change list holds %d changes, want the two puts that worked", n)
	}
	tx.Rollback()
	tooBig(t, "Apply", s.Apply(format.Change{Op: format.Put, Key: "docs:1", Fields: []format.Field{field("b", value.Bytes(over))}}))

	// A batch that holds a longer value is damage, naming the batch and the
	// change. ApplyBatch keeps none of it, and LoadBatch the changes before.
	batch := []format.Change{
		{Op: format.Put, Key: "docs:6", Fields: []format.Field{field("n", value.Int(6))}},
		{Op: format.Put, Key: "docs:1", Fields: []format.Field{field("body", value.Text(over))}},
	}
	for _, load := range []bool{false, true} {
		s := New()
		ok(t, s.LoadBatch(1, []format.Change{{Op: format.CreateTable, Table: "docs", Names: []string{"body", "n"}}}))
		before := dump(s)
		apply, want := s.ApplyBatch, before
		if load {
			apply = s.LoadBatch
		}
		err := apply(2, slices.Clone(batch))
		var d *errs.Damage
		if !errors.As(err, &d) || d.Batch != 2 || d.Reason != "change 2: string or blob too big" {
			t.Fatalf("a batch holding a value one byte over the limit gave %v, load %v", err, load)
		}
		if load {
			if _, err := s.Get("docs:6"); err != nil {
				t.Errorf("LoadBatch lost the change before the damage: %v", err)
			}
			continue
		}
		if after := dump(s); after != want {
			t.Errorf("ApplyBatch kept part of a damaged batch:\n%s\nwas\n%s", after, want)
		}
	}
}

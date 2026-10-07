// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package format

import (
	"encoding/binary"
	"math"
	"reflect"
	"testing"

	val "github.com/hypercrux/hypercrux/beta/internal/value"
)

func TestTheSixChanges(t *testing.T) {
	for _, c := range []struct {
		c    Change
		show string
	}{
		{Change{Op: CreateTable, Table: "docs", Size: 3, Names: []string{"title", "vec"}}, `create table docs, vector size 3, fields ["title" "vec"]`},
		{Change{Op: Put, Key: "docs:1", Fields: []Field{{"n", val.Int(1)}, {"title", val.Null()}}}, "put docs:1 {n: int 1, title: null}"},
		{Change{Op: Delete, Key: "docs:1"}, "delete docs:1"},
		{Change{Op: Link, Key: "docs:1", Type: "cites", To: "docs:2"}, "link docs:1 -cites-> docs:2"},
		{Change{Op: Unlink, Key: "docs:1", Type: "cites", To: "docs:2"}, "unlink docs:1 -cites-> docs:2"},
		{Change{Op: Drop, Table: "docs"}, "drop table docs"},
		{Change{}, "Op(0)"},
	} {
		if got := c.c.String(); got != c.show {
			t.Errorf("String() = %q, want %q", got, c.show)
		}
	}
}

// TestChangesCarryTheFixtures takes every change in the fixtures, as
// fixtures_test.go reads them, through a Change and back, and checks that
// nothing is lost: every name, size and field list, and every value's kind
// and bits. A vector's bits must be the bytes the file holds. The codec's
// tests, in codec_test.go, read the fixtures with the decoder and check it
// against the same reader, which stays apart from the codec.
func TestChangesCarryTheFixtures(t *testing.T) {
	var all []change
	for _, name := range fixtures {
		b := readFixture(t, name)
		switch name[:5] {
		case "batch":
			bt, err := readBatch(b, 1)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			all = append(all, bt.changes...)
		case "file-":
			f, err := readFile(b)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			for _, bt := range f.batches {
				all = append(all, bt.changes...)
			}
		}
	}
	ops, kinds := map[Op]bool{}, map[val.Kind]bool{}
	for _, c := range all {
		ch := toChange(c)
		ops[ch.Op] = true
		if back := fromChange(ch); !reflect.DeepEqual(back, c) {
			t.Errorf("%v came back from a Change as %+v, where the file holds %+v", ch, back, c)
		}
		for i, f := range ch.Fields {
			kinds[f.Value.Kind()] = true
			if f.Value.Kind() != val.KindVector {
				continue
			}
			var want []byte
			for _, bits := range c.puts[i].v.vec {
				want = binary.LittleEndian.AppendUint32(want, bits)
			}
			if f.Value.Raw() != string(want) {
				t.Errorf("%s's vector holds % x, where the file holds % x", ch.Key, f.Value.Raw(), want)
			}
		}
	}
	if len(ops) != 6 || len(kinds) != 6 {
		t.Errorf("the fixtures gave %d kinds of change and %d kinds of value, where there are 6 of each", len(ops), len(kinds))
	}
}

var opOf = map[byte]Op{'T': CreateTable, 'P': Put, 'D': Delete, 'L': Link, 'U': Unlink, 'X': Drop}

func toChange(c change) Change {
	ch := Change{Op: opOf[c.kind]}
	switch ch.Op {
	case CreateTable, Drop:
		ch.Table, ch.Size, ch.Names = c.name, int(c.size), c.fields
	case Put:
		ch.Key = c.name
		for _, f := range c.puts {
			ch.Fields = append(ch.Fields, Field{Name: f.name, Value: toValue(f.v)})
		}
	case Delete:
		ch.Key = c.name
	case Link, Unlink:
		ch.Key, ch.Type, ch.To = c.name, c.typ, c.to
	}
	return ch
}

func fromChange(ch Change) change {
	var c change
	for k, op := range opOf {
		if op == ch.Op {
			c.kind = k
		}
	}
	switch ch.Op {
	case CreateTable, Drop:
		c.name, c.size, c.fields = ch.Table, uint32(ch.Size), ch.Names
	case Put:
		c.name = ch.Key
		for _, f := range ch.Fields {
			c.puts = append(c.puts, field{name: f.Name, v: fromValue(f.Value)})
		}
	case Delete:
		c.name = ch.Key
	case Link, Unlink:
		c.name, c.typ, c.to = ch.Key, ch.Type, ch.To
	}
	return c
}

func toValue(v value) val.Value {
	switch v.kind {
	case 'i':
		return val.Int(v.i)
	case 'r':
		return val.Real(math.Float64frombits(v.bits))
	case 't':
		return val.Text(v.data)
	case 'b':
		return val.Bytes(v.data)
	case 'v':
		f := make([]float32, len(v.vec))
		for i, bits := range v.vec {
			f[i] = math.Float32frombits(bits)
		}
		return val.Vector(f)
	}
	return val.Null()
}

func fromValue(v val.Value) value {
	switch v.Kind() {
	case val.KindInt:
		return value{kind: 'i', i: v.Int()}
	case val.KindReal:
		return value{kind: 'r', bits: math.Float64bits(v.Real())}
	case val.KindText:
		return value{kind: 't', data: v.Text()}
	case val.KindBytes:
		return value{kind: 'b', data: v.Raw()}
	case val.KindVector:
		var bits []uint32
		for _, x := range v.Vector() {
			bits = append(bits, math.Float32bits(x))
		}
		return value{kind: 'v', vec: bits}
	}
	return value{kind: 'n'}
}

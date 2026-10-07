// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query_test

import (
	"errors"
	"testing"

	"github.com/hypercrux/hypercrux/beta/internal/query"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// stubRows hands out fixed rows and then an error, if it has one, so the
// operator iterator compiles against something until Q4 writes the
// operators. If Rows changes, it stops compiling, which is the reminder
// that every operator and caller changes with it.
type stubRows struct {
	rows   [][]value.Value
	at     int
	end    error
	err    error
	closed bool
}

var _ query.Rows = (*stubRows)(nil)

func (s *stubRows) Next() bool {
	if s.closed || s.err != nil {
		return false
	}
	if s.at == len(s.rows) {
		s.err = s.end
		return false
	}
	s.at++
	return true
}

func (s *stubRows) Row() []value.Value { return s.rows[s.at-1] }
func (s *stubRows) Err() error         { return s.err }
func (s *stubRows) Close()             { s.closed = true }

// drain reads rows the way every caller does.
func drain(r query.Rows) ([][]value.Value, error) {
	defer r.Close()
	var out [][]value.Value
	for r.Next() {
		out = append(out, append([]value.Value(nil), r.Row()...))
	}
	return out, r.Err()
}

func TestTheOperatorIteratorCompiles(t *testing.T) {
	rows := [][]value.Value{
		{value.Text("docs:1"), value.Int(12)},
		{value.Text("docs:2"), value.Null()},
	}
	got, err := drain(&stubRows{rows: rows})
	if err != nil || len(got) != 2 || got[1][0] != value.Text("docs:2") || !got[1][1].IsNull() {
		t.Errorf("drained %v, %v", got, err)
	}

	boom := errors.New("boom")
	got, err = drain(&stubRows{rows: rows, end: boom})
	if !errors.Is(err, boom) || len(got) != 2 {
		t.Errorf("an error at the end gave %v, %v", got, err)
	}

	s := &stubRows{rows: rows}
	s.Next()
	s.Close()
	s.Close()
	if s.Next() || s.Err() != nil {
		t.Error("rows went on after Close")
	}
}

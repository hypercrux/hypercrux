// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux

import (
	"time"

	"github.com/hypercrux/hypercrux/beta/internal/query"
	"github.com/hypercrux/hypercrux/beta/internal/store"
	"github.com/hypercrux/hypercrux/beta/internal/value"
)

// The engine (G4) runs each statement the driver hands it: a SELECT through
// Q5's planner, query.Query, and an INSERT, UPDATE or DELETE through Q6's
// writes, query.Exec. The driver has parsed the statement and taken exactly
// one argument for each ? mark, and it hands over a read of the copy or the
// transaction for a SELECT, and the transaction for a write, which it
// marks first and takes back to the mark when the write fails (driver.go).
//
// Each call is one statement, and reads its moment once, from the DB's
// clock, before anything of the statement runs, so all its rows and all its
// dates see the same moment, inside an Update too, where each statement has
// a moment of its own (SQL.md, "Dates"). The planner's values never share the
// store's memory, since a vector field reads as a copy of its slot, and the
// driver copies every row out before the read ends, with each vector and
// each bytes value in a []byte of its own as it hands them on.
//
// Errors come back as the planner and the writes give them, so their kinds
// follow the statement (SQL.md, "Errors and their kinds"): one that wraps
// ErrInvalid for a rule a write breaks, a function's refusal in a write
// among them, and a plain one for everything else, a SELECT's errors and
// Nearest's filter's included. Through SQL() the driver takes ErrInvalid
// off (plain).

// sqlEngine is the engine behind every DB's driver.
type sqlEngine struct{ db *DB }

func (e sqlEngine) query(r store.Reader, s *query.Select, args []value.Value) ([]string, query.Rows, error) {
	return query.Query(r, s, &query.Frame{Args: args, Now: e.db.moment()})
}

func (e sqlEngine) write(tx *store.Tx, s query.Statement, args []value.Value) (int64, error) {
	return query.Exec(tx, s, &query.Frame{Args: args, Now: e.db.moment()})
}

// moment is the moment a statement reads once for 'now', and so is a
// search's filter: the DB's clock, which is time.Now unless a test has set
// one of its own. Dates are in UTC and count whole milliseconds, which the
// date functions see to (Q2).
func (db *DB) moment() time.Time {
	now := time.Now
	if db.clock != nil {
		now = db.clock
	}
	if plant == "hypercrux/moment-kept" {
		if first := db.first.Load(); first != nil {
			return *first
		}
		t := now()
		db.first.CompareAndSwap(nil, &t)
		return *db.first.Load()
	}
	return now()
}

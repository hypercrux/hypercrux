// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package hypercrux is a small database where every record can be reached
// four ways: by key, by SQL, by following links and by similarity.
//
// A HyperCrux file is an ordinary SQLite database. Each record is a row in a
// table and has a key such as "docs:7", where the part before the colon names
// the table. A record holds fields (the table's columns), links to other
// records, and, if you want, a vector. All four live in the same file and
// change in the same transaction, so they never disagree: delete a record and
// its links go with it, in any program that writes the file, because the
// rules are SQLite triggers stored in the file itself.
//
//	db, err := hypercrux.Open("notes.db")
//	if err != nil {
//		log.Fatal(err)
//	}
//	defer db.Close()
//
//	// By key.
//	db.Put("docs:7", hypercrux.Fields{"title": "Q3 plan", "status": "open",
//		"vec": hypercrux.Vector{0.12, 0.80, 0.05}})
//	db.Put("customer:42", hypercrux.Fields{"name": "Dana"})
//
//	// By link.
//	db.Link("customer:42", "owns", "docs:7")
//	steps, err := db.Walk("customer:42", hypercrux.Out, "", 2)
//
//	// By similarity.
//	hits, err := db.Nearest("docs", question, 10, "status = ?", "open")
//
//	// By SQL, crossing all of them in one statement.
//	rows, err := db.Query(`
//		SELECT d.key, d.title
//		FROM json_each(walk('customer:42', 2)) w
//		JOIN docs d ON d.key = w.value
//		WHERE d.status = 'open'
//		ORDER BY distance(d.vec, ?)
//		LIMIT 10`, question)
//
// Writes that belong together go in one transaction with Update. Vector
// search is exact: every candidate is compared, so filters and transactions
// need no special handling, at the cost of a size ceiling measured in the
// repository's test results.
//
// The package links SQLite through github.com/mattn/go-sqlite3, so building
// it needs cgo and a C compiler.
package hypercrux

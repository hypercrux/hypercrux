// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

// This program puts a few records in a HyperCrux file and reads them back
// all four ways: by key, by SQL, by following links and by similarity.
// The vectors are made up and have three values; real ones come from an
// embedding model and have hundreds.
//
//	go run ./examples/go
package main

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/hypercrux/hypercrux"
)

func main() {
	dir, err := os.MkdirTemp("", "hypercrux-example-")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(dir)

	db, err := hypercrux.Open(filepath.Join(dir, "notes.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// One transaction: the records, their vectors and their links commit
	// together or not at all.
	err = db.Update(func(tx *hypercrux.Tx) error {
		puts := []struct {
			key    string
			fields hypercrux.Fields
		}{
			{"customer:42", hypercrux.Fields{"name": "Dana"}},
			{"docs:1", hypercrux.Fields{"title": "Q3 plan", "status": "open", "vec": hypercrux.Vector{0.9, 0.1, 0}}},
			{"docs:2", hypercrux.Fields{"title": "Hiring notes", "status": "done", "vec": hypercrux.Vector{0.1, 0.9, 0.1}}},
			{"docs:3", hypercrux.Fields{"title": "Q3 budget", "status": "open", "vec": hypercrux.Vector{0.8, 0.2, 0.1}}},
		}
		for _, p := range puts {
			if err := tx.Put(p.key, p.fields); err != nil {
				return err
			}
		}
		for _, l := range []hypercrux.Link{
			{From: "customer:42", Type: "owns", To: "docs:1"},
			{From: "customer:42", Type: "owns", To: "docs:2"},
			{From: "docs:1", Type: "cites", To: "docs:3"},
		} {
			if err := tx.Link(l.From, l.Type, l.To); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		log.Fatal(err)
	}

	// By key.
	f, err := db.Get("docs:1")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println("by key:", f["title"], f["status"], f["vec"])

	// By link.
	steps, err := db.Walk("customer:42", hypercrux.Out, "", 2)
	if err != nil {
		log.Fatal(err)
	}
	for _, s := range steps {
		fmt.Printf("by link: %s, %d away\n", s.Key, s.Depth)
	}

	// By similarity.
	question := hypercrux.Vector{1, 0, 0}
	hits, err := db.Nearest("docs", question, 2, "status = ?", "open")
	if err != nil {
		log.Fatal(err)
	}
	for _, h := range hits {
		fmt.Printf("by similarity: %s at %.4f\n", h.Key, h.Distance)
	}

	// By SQL, crossing all of them: documents within two links of the
	// customer, still open, closest to the question first.
	rows, err := db.Query(`
		SELECT d.key, d.title
		FROM json_each(walk('customer:42', 2)) w
		JOIN docs d ON d.key = w.value
		WHERE d.status = 'open' AND d.vec IS NOT NULL
		ORDER BY distance(d.vec, ?)
		LIMIT 10`, question)
	if err != nil {
		log.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var key, title string
		if err := rows.Scan(&key, &title); err != nil {
			log.Fatal(err)
		}
		fmt.Println("by SQL:", key, title)
	}

	// Deleting a record takes its links with it.
	if err := db.Delete("docs:1"); err != nil {
		log.Fatal(err)
	}
	rep, err := db.Check()
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("check: %d records, %d links, ok %v\n", rep.Records, rep.Links, rep.OK())
}

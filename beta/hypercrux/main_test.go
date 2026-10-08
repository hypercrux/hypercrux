// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package hypercrux_test

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"testing"

	hc "github.com/hypercrux/hypercrux/beta/hypercrux"
)

// Some tests start a copy of the test binary as a process of its own on one
// database. TestMain sends a copy to its role instead of the tests.
func TestMain(m *testing.M) {
	if role := os.Getenv("HYPERCRUX_BETA_ROLE"); role != "" {
		os.Exit(helper(role, os.Getenv("HYPERCRUX_BETA_PATH")))
	}
	os.Exit(m.Run())
}

// helper runs a helper process's role on the database at path:
//
//   - turns: opens the database, prints "open", then for each number k it
//     reads on its standard input, reads a:k, which the other process has
//     just committed, puts b:k, and then prints "saw k", or what it found
//     instead of a:k. At the end of its input it prints what Check counts,
//     and closes the database.
func helper(role, path string) int {
	fail := func(err error) int {
		fmt.Fprintln(os.Stderr, "helper:", role+":", err)
		return 1
	}
	switch role {
	case "turns":
		db, err := hc.Open(path)
		if err != nil {
			return fail(err)
		}
		fmt.Println("open")
		in := bufio.NewScanner(os.Stdin)
		for in.Scan() {
			k, err := strconv.Atoi(in.Text())
			if err != nil {
				return fail(err)
			}
			said := fmt.Sprintf("saw %d", k)
			if f, err := db.Get("a:" + strconv.Itoa(k)); err != nil || f["n"] != int64(k) {
				said = fmt.Sprintf("a:%d gave %v, %v", k, f, err)
			}
			if err := db.Put("b:"+strconv.Itoa(k), hc.Fields{"n": k}); err != nil {
				return fail(err)
			}
			fmt.Println(said)
		}
		rep, err := db.Check()
		if err != nil {
			return fail(err)
		}
		fmt.Printf("records %d\n", rep.Records)
		if err := db.Close(); err != nil {
			return fail(err)
		}
	default:
		return fail(fmt.Errorf("no role is called %q", role))
	}
	return 0
}

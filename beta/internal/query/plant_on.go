// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux && hypercrux_planted

package query

import "os"

// A build with the hypercrux_planted tag can switch on one of this
// package's planted bugs, named in HYPERCRUX_PLANT. scripts/planted.sh runs
// the tests with each bug listed in beta/plants.txt, and every one must
// make them fail. A name this package doesn't know switches nothing on.
//
//   - query/and-or-one-level: AND groups at OR's level, so a OR b AND c is
//     (a OR b) AND c.
//   - query/between-takes-comparisons: BETWEEN's second bound takes the
//     operators of level 4, so x BETWEEN 1 AND 2 = 3 is x BETWEEN 1 AND
//     (2 = 3).
//   - query/sign-takes-operators: a sign's operand takes ||, so -2 || 3 is
//     -(2 || 3).
//   - query/plus-kept: a sign in front of a plus sign's expression keeps
//     the plus sign, so -+1 is -(+1), where SQLite negates the literal.
//   - query/left-as-alias: LEFT is taken as a source's bare alias, so
//     FROM docs left JOIN ... is an inner join where SQLite reads a LEFT
//     JOIN.
//   - query/params-from-one: the ? marks are numbered from 1.
//   - query/text-without-comments: a result column's text stops at its
//     last token, leaving out a comment SQLite keeps in the name.
//   - query/aggregate-in-where: an aggregate is taken in WHERE.
//   - query/record-key-takes-or: the one-record subquery's key takes OR,
//     so (SELECT f FROM t WHERE key = 1 OR 1) passes for the one-record
//     subquery, where SQLite reads WHERE (key = 1) OR 1.
//   - query/is-not-printed-bare: x IS (NOT y) prints as x IS NOT y, which
//     parses as IS NOT.
//   - query/slash-star-comment: a /* that ends the text is taken as a
//     comment, where SQLite reads a slash and a star.
var plant = func() string {
	switch p := os.Getenv("HYPERCRUX_PLANT"); p {
	case "query/and-or-one-level", "query/between-takes-comparisons", "query/sign-takes-operators",
		"query/plus-kept", "query/left-as-alias", "query/params-from-one", "query/text-without-comments",
		"query/aggregate-in-where", "query/record-key-takes-or", "query/is-not-printed-bare",
		"query/slash-star-comment":
		return p
	}
	return ""
}()

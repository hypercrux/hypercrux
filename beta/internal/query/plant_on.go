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
//
// The evaluator's (Q1):
//
//   - query/real-text-shortest: a real's text is Go's shortest form, so
//     5e-324 gives 5e-324 where SQLite writes 4.9406564584124654e-324.
//   - query/text-reads-as-number: a comparison without affinity turns text
//     into a number, so '1' = 1 is true.
//   - query/plus-keeps-affinity: a plus sign keeps its operand's affinity,
//     so +CAST(5 AS INTEGER) = '5' is true.
//   - query/in-takes-item-affinity: IN takes its first item's affinity as
//     well as x's, so 5 IN (CAST('5' AS TEXT), 6) is true.
//   - query/in-list-stops-early: a list of three constant items or more is
//     worked out one item at a time after x, so 1 IN (1, 2, E) stops
//     before E, where SQLite works out every item first.
//   - query/in-record-is-a-list: x IN ((SELECT ...)) is a list of one
//     item, so no record gives NULL where SQLite's empty set gives 0.
//   - query/and-or-sees-signs: AND and OR settle on a literal with a sign
//     in front of it, so -0 AND E skips E.
//   - query/minus-zero-lost: a minus sign in front of a literal works as
//     0 - x, so -0.0 is 0.0.
//   - query/div-rounds-down: integer division rounds down, so -7 / 2 is
//     -4.
//   - query/nan-kept: arithmetic keeps a NaN, so 1e999 - 1e999 isn't
//     NULL.
//   - query/min-keeps-earliest: min() keeps the earliest of tied
//     arguments, so min(1, 1.0) is 1.
//   - query/substr-zero-is-one: substr()'s position 0 counts as 1, so
//     substr('abc', 0, 2) is 'ab'.
//   - query/like-folds-unicode: LIKE folds the case of bytes above ASCII,
//     so 'é' LIKE 'É' matches.
//   - query/where-or-works-out-both: a condition's OR works out its second
//     side when the first is true.
//   - query/time-in-utc: a time.Time argument is written in UTC, where
//     go-sqlite3 keeps the time's own zone.
var plant = func() string {
	switch p := os.Getenv("HYPERCRUX_PLANT"); p {
	case "query/and-or-one-level", "query/between-takes-comparisons", "query/sign-takes-operators",
		"query/plus-kept", "query/left-as-alias", "query/params-from-one", "query/text-without-comments",
		"query/aggregate-in-where", "query/record-key-takes-or", "query/is-not-printed-bare",
		"query/slash-star-comment",
		"query/real-text-shortest", "query/text-reads-as-number", "query/plus-keeps-affinity",
		"query/in-takes-item-affinity", "query/in-list-stops-early", "query/in-record-is-a-list",
		"query/and-or-sees-signs", "query/minus-zero-lost", "query/div-rounds-down", "query/nan-kept",
		"query/min-keeps-earliest", "query/substr-zero-is-one", "query/like-folds-unicode",
		"query/where-or-works-out-both", "query/time-in-utc":
		return p
	}
	return ""
}()

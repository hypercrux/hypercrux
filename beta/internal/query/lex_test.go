// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import (
	"fmt"
	"strings"
	"testing"
)

// tokens splits text as the parser sees it: each token's kind and text,
// spaces and comments left out.
func tokens(text string) string {
	l := lexer{text: text}
	var parts []string
	for {
		t := l.next()
		if t.kind == tEOF {
			if t.pos != len(text) {
				parts = append(parts, fmt.Sprintf("EOF at %d", t.pos))
			}
			return strings.Join(parts, " ")
		}
		parts = append(parts, kindName[t.kind]+":"+text[t.pos:t.end])
	}
}

var kindName = map[tokKind]string{
	tName: "name", tKeyword: "kw", tQuoted: "quoted", tInt: "int", tReal: "real", tText: "text",
	tBlob: "blob", tParam: "param", tNumbered: "numbered", tNamed: "named", tBackquote: "backquote",
	tBracket: "bracket", tHex: "hex", tSeparated: "separated", tLP: "(", tRP: ")", tComma: ",",
	tSemi: ";", tDot: ".", tStar: "*", tSlash: "/", tRem: "%", tPlus: "+", tMinus: "-",
	tConcat: "||", tEq: "=", tNe: "!=", tLt: "<", tLe: "<=", tGt: ">", tGe: ">=", tBitAnd: "&",
	tBitOr: "|", tBitNot: "~", tShift: "shift", tArrow: "arrow", tIllegal: "illegal",
}

// TestTokens follows SQLite's sqlite3GetToken, case by case.
func TestTokens(t *testing.T) {
	for _, c := range []struct{ text, toks string }{
		// Spaces and comments.
		{" \t\n\f\r", ""},
		{"a \v b", "name:a name:b"},
		{"a\vb", "name:a illegal:\v name:b"},
		{"\v", "illegal:\v"},
		{"a -- comment\nb", "name:a name:b"},
		{"a -- comment", "name:a"},
		{"a /* comment */ b", "name:a name:b"},
		{"a /* comment", "name:a"},
		{"a /**/b", "name:a name:b"},
		{"a /*/ b", "name:a"},
		{"a /*", "name:a /:/ *:*"},
		{"a /", "name:a /:/"},
		{"- -1", "-:- -:- int:1"},
		{"--1", ""},
		{"\xef\xbb\xbfa", "name:a"},
		{"a\xef\xbb\xbf", "name:a\xef\xbb\xbf"},
		{"a \xef\xbb\xbf b", "name:a name:b"},
		{"\xef\xbb", "name:\xef\xbb"},
		// Names and keywords.
		{"select Select SELECT", "kw:select kw:Select kw:SELECT"},
		{"selectx select1 select$ select_", "name:selectx name:select1 name:select$ name:select_"},
		{"current_date CURRENT_TIMESTAMP", "kw:current_date kw:CURRENT_TIMESTAMP"},
		{"desc asc by like inner offset glob", "name:desc name:asc name:by name:like name:inner name:offset name:glob"},
		{"_a a_ a1 a$b é ʼAmir \x80", "name:_a name:a_ name:a1 name:a$b name:é name:ʼAmir name:\x80"},
		{"x xy x1 X", "name:x name:xy name:x1 name:X"},
		{`"a" "a""b" "" "select"`, `quoted:"a" quoted:"a""b" quoted:"" quoted:"select"`},
		{`"abc`, `illegal:"abc`},
		{"`a` `a``b`", "backquote:`a` backquote:`a``b`"},
		{"`a", "illegal:`a"},
		{"[a] [a b]", "bracket:[a] bracket:[a b]"},
		{"[a", "illegal:[a"},
		// Literals.
		{"'a' '' 'it''s' 'a\nb'", "text:'a' text:'' text:'it''s' text:'a\nb'"},
		{"'abc", "illegal:'abc"},
		{"'a''", "illegal:'a''"},
		{"x'' X'0a' x'00FF'", "blob:x'' blob:X'0a' blob:x'00FF'"},
		{"x'0' x'0g' x'00", "illegal:x'0' illegal:x'0g' illegal:x'00"},
		{"x'0g'a", "illegal:x'0g' name:a"},
		{"x'00'a", "blob:x'00' name:a"},
		{"'a'b", "text:'a' name:b"},
		{"0 1 42 007 9223372036854775808", "int:0 int:1 int:42 int:007 int:9223372036854775808"},
		{"1.5 .5 5. 1e10 1E10 2.5e-3 1.e5 1e+5 0.0", "real:1.5 real:.5 real:5. real:1e10 real:1E10 real:2.5e-3 real:1.e5 real:1e+5 real:0.0"},
		{"12abc 1e 1e+ 1x 1.5a 1$ 1é", "illegal:12abc illegal:1e illegal:1e +:+ illegal:1x illegal:1.5a illegal:1$ illegal:1é"},
		{"1.5.3", "real:1.5 real:.3"},
		{"1..2", "real:1. real:.2"},
		{"1e5e5", "illegal:1e5e5"},
		{". .x", ".:. .:. name:x"},
		{"0x10 0XaB 0x1_0", "hex:0x10 hex:0XaB hex:0x1_0"},
		{"0x 0x1g 0xg", "illegal:0x illegal:0x1g illegal:0xg"},
		{"1_000 1_ 1._5 1.5_0 1e1_0", "separated:1_000 separated:1_ separated:1._5 separated:1.5_0 separated:1e1_0"},
		// Parameters.
		{"? ?1 ?123 ?a", "param:? numbered:?1 numbered:?123 param:? name:a"},
		{":a @a $a #a :a1 $a$b", "named::a named:@a named:$a named:#a named::a1 named:$a$b"},
		{"$a::b $a(x) $a(x", "named:$a::b named:$a(x) illegal:$a(x"},
		{": @ $ # :1", "illegal:: illegal:@ illegal:$ illegal:# named::1"},
		{"$(x)", "illegal:$ (:( name:x ):)"},
		// Symbols.
		{"( ) , ; . * / % + - || = == != <> < <= > >=", "(:( ):) ,:, ;:; .:. *:* /:/ %:% +:+ -:- ||:|| =:= =:== !=:!= !=:<> <:< <=:<= >:> >=:>="},
		{"& | ~ << >> -> ->>", "&:& |:| ~:~ shift:<< shift:>> arrow:-> arrow:->>"},
		{"a->>b a-->b", "name:a arrow:->> name:b name:a"},
		{"! \\ ^ { } ] \x01 \x7f", "illegal:! illegal:\\ illegal:^ illegal:{ illegal:} illegal:] illegal:\x01 illegal:\x7f"},
		{"!=!", "!=:!= illegal:!"},
		{"<<= >>=", "shift:<< =:= shift:>> =:="},
		{"===", "=:== =:="},
	} {
		if got := tokens(c.text); got != c.toks {
			t.Errorf("%q gives\n  %q, want\n  %q", c.text, got, c.toks)
		}
	}
}

// TestTheTextEndsAtNUL: SQLite reads the text as a C string, so it ends at
// its first NUL byte.
func TestTheTextEndsAtNUL(t *testing.T) {
	for q, want := range map[string]string{
		"SELECT 1\x00garbage":      "(select 1) params=0",
		"SELECT 1;\x00SELECT 2":    "(select 1) params=0",
		"SELECT 1 --x\x00y":        "(select 1) params=0",
		"SELECT ?\x00, ?":          "(select ?0) params=1",
		"\x00":                     "",
		"SELECT 'a\x00b'":          "",
		"SELECT x FROM t\x00WHERE": "(select x (from t)) params=0",
	} {
		s, err := Parse(q)
		switch {
		case want == "" && err == nil:
			t.Errorf("%q parses", q)
		case want != "" && err != nil:
			t.Errorf("%q: %v", q, err)
		case want != "" && dump(s) != want:
			t.Errorf("%q gives %s", q, dump(s))
		}
	}
	// 0.x puts a condition in parentheses at the end of its query, which a
	// NUL cuts short.
	for _, q := range []string{"a = ?\x00 OR b = ?", "1\x00", "\x00", " \x00 "} {
		if e, _, err := ParseCondition(q); err == nil || err.Error() != "incomplete input" {
			t.Errorf("the condition %q gives %s, %v", q, dumpExpr(e), err)
		}
	}
}

func TestUnquote(t *testing.T) {
	for in, want := range map[string]string{`""`: "", `"a"`: "a", `"a""b"`: `a"b`, `""""`: `"`, "''": "", "'it''s'": "it's"} {
		if got := unquote(in); got != want {
			t.Errorf("unquote(%s) = %q", in, got)
		}
	}
	if got := unhex("x'00Ff7a'"); got != "\x00\xffz" {
		t.Errorf("unhex = %q", got)
	}
}

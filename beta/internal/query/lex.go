// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import "strings"

// The lexer splits the text into tokens as SQLite's sqlite3GetToken does,
// so that a statement the Beta takes is read the same way by 0.x. It knows
// SQLite's tokens that are outside the subset too, such as hex numbers and
// named parameters, so the parser can refuse each with a message that says
// what isn't taken, where SQLite would run it.

type tokKind uint8

const (
	tEOF     tokKind = iota
	tName            // a bare name, perhaps one of the words that are keywords only in place
	tKeyword         // a reserved word, in word
	tQuoted          // a name in double quotes
	tInt             // digits
	tReal            // digits with a point or an exponent
	tText            // '...'
	tBlob            // x'...'
	tParam           // ?

	// SQLite's tokens that the subset leaves out.
	tNumbered  // ?NNN
	tNamed     // :name, @name, $name or #name
	tBackquote // `name`
	tBracket   // [name]
	tHex       // 0x10
	tSeparated // a number with digit separators, 1_000

	tLP     // (
	tRP     // )
	tComma  // ,
	tSemi   // ;
	tDot    // .
	tStar   // *
	tSlash  // /
	tRem    // %
	tPlus   // +
	tMinus  // -
	tConcat // ||
	tEq     // = or ==
	tNe     // != or <>
	tLt     // <
	tLe     // <=
	tGt     // >
	tGe     // >=

	// SQLite's operators that the subset leaves out.
	tBitAnd // &
	tBitOr  // |
	tBitNot // ~
	tShift  // << or >>
	tArrow  // -> or ->>

	tIllegal // a token SQLite doesn't know: "unrecognized token"
)

// token is one token: its bytes are text[pos:end].
type token struct {
	kind     tokKind
	pos, end int
	word     string // a keyword's word, in upper case
}

// lexer hands out the tokens of text one at a time, skipping spaces and
// comments, which SQLite never hands its parser either.
type lexer struct {
	text string
	pos  int
}

// isSpace is sqlite3Isspace: the space, tab, line feed, vertical tab, form
// feed and carriage return. A run of spaces starts with one of them other
// than the vertical tab, which SQLite's tokenizer doesn't take at the start
// of a token.
func isSpace(c byte) bool { return c == ' ' || '\t' <= c && c <= '\r' }

// isIDChar is SQLite's IdChar: what may follow the first character of a
// bare name.
func isIDChar(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '_' || c == '$' || c >= 0x80
}

func isDigit(c byte) bool { return '0' <= c && c <= '9' }

func isHex(c byte) bool { return isDigit(c) || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F' }

// at returns the byte at i, or 0 past the end, as SQLite reads past the end
// of the text into its NUL.
func (l *lexer) at(i int) byte {
	if i < len(l.text) {
		return l.text[i]
	}
	return 0
}

// next returns the next token, after any spaces and comments.
func (l *lexer) next() token {
	for {
		start := l.pos
		if start >= len(l.text) {
			return token{kind: tEOF, pos: len(l.text), end: len(l.text)}
		}
		c := l.text[start]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\f' || c == '\r':
			i := start + 1
			for i < len(l.text) && isSpace(l.text[i]) {
				i++
			}
			l.pos = i
			continue
		case c == 0xef && l.at(start+1) == 0xbb && l.at(start+2) == 0xbf:
			// A byte order mark at the start of a token counts as a space.
			l.pos = start + 3
			continue
		case c == '-' && l.at(start+1) == '-':
			i := strings.IndexByte(l.text[start:], '\n')
			if i < 0 {
				l.pos = len(l.text)
			} else {
				l.pos = start + i
			}
			continue
		case c == '/' && l.at(start+1) == '*' && (start+2 < len(l.text) || plant == "query/slash-star-comment"):
			// A comment runs to */ or to the end of the text. A /* that
			// ends the text is a slash and a star, as in SQLite.
			i := strings.Index(l.text[start+2:], "*/")
			if i < 0 {
				l.pos = len(l.text)
			} else {
				l.pos = start + 2 + i + 2
			}
			continue
		}
		t := l.token(start)
		l.pos = t.end
		return t
	}
}

// token reads the token that starts at start, which isn't a space or a
// comment.
func (l *lexer) token(start int) token {
	c := l.text[start]
	t := token{pos: start, end: start + 1}
	one := func(k tokKind) token { t.kind = k; return t }
	two := func(k tokKind) token { t.kind, t.end = k, start+2; return t }
	switch c {
	case '(':
		return one(tLP)
	case ')':
		return one(tRP)
	case ',':
		return one(tComma)
	case ';':
		return one(tSemi)
	case '*':
		return one(tStar)
	case '/':
		return one(tSlash)
	case '%':
		return one(tRem)
	case '+':
		return one(tPlus)
	case '&':
		return one(tBitAnd)
	case '~':
		return one(tBitNot)
	case '-':
		if l.at(start+1) == '>' {
			t.kind, t.end = tArrow, start+2
			if l.at(start+2) == '>' {
				t.end++
			}
			return t
		}
		return one(tMinus)
	case '=':
		if l.at(start+1) == '=' {
			return two(tEq)
		}
		return one(tEq)
	case '<':
		switch l.at(start + 1) {
		case '=':
			return two(tLe)
		case '>':
			return two(tNe)
		case '<':
			return two(tShift)
		}
		return one(tLt)
	case '>':
		switch l.at(start + 1) {
		case '=':
			return two(tGe)
		case '>':
			return two(tShift)
		}
		return one(tGt)
	case '!':
		if l.at(start+1) == '=' {
			return two(tNe)
		}
		return one(tIllegal)
	case '|':
		if l.at(start+1) == '|' {
			return two(tConcat)
		}
		return one(tBitOr)
	case '\'', '"', '`':
		return l.quoted(start)
	case '[':
		i := strings.IndexByte(l.text[start:], ']')
		if i < 0 {
			t.kind, t.end = tIllegal, len(l.text)
			return t
		}
		t.kind, t.end = tBracket, start+i+1
		return t
	case '?':
		i := start + 1
		for isDigit(l.at(i)) {
			i++
		}
		t.kind, t.end = tParam, i
		if i > start+1 {
			t.kind = tNumbered
		}
		return t
	case '$', '@', ':', '#':
		return l.named(start)
	case '.':
		if !isDigit(l.at(start + 1)) {
			return one(tDot)
		}
		return l.number(start)
	}
	switch {
	case isDigit(c):
		return l.number(start)
	case (c == 'x' || c == 'X') && l.at(start+1) == '\'':
		return l.blob(start)
	case 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || c == '_' || c >= 0x80:
		i := start + 1
		for isIDChar(l.at(i)) {
			i++
		}
		t.end = i
		if w, ok := keyword(l.text[start:i]); ok {
			t.kind, t.word = tKeyword, w
		} else {
			t.kind = tName
		}
		return t
	}
	// Control characters, \, ^, {, }, ] and the like.
	return one(tIllegal)
}

// quoted reads text in single quotes, a name in double quotes or a name in
// backquotes, where a doubled quote stands for one. Without its closing
// quote it runs to the end of the text and is a token SQLite doesn't know.
func (l *lexer) quoted(start int) token {
	q := l.text[start]
	i := start + 1
	for {
		j := strings.IndexByte(l.text[i:], q)
		if j < 0 {
			return token{kind: tIllegal, pos: start, end: len(l.text)}
		}
		i += j + 1
		if l.at(i) != q {
			break
		}
		i++
	}
	t := token{pos: start, end: i}
	switch q {
	case '\'':
		t.kind = tText
	case '"':
		t.kind = tQuoted
	default:
		t.kind = tBackquote
	}
	return t
}

// blob reads x'...': an even number of hex digits and a closing quote, or
// else a token SQLite doesn't know, which runs to the next quote.
func (l *lexer) blob(start int) token {
	i := start + 2
	for isHex(l.at(i)) {
		i++
	}
	if l.at(i) == '\'' && (i-start-2)%2 == 0 {
		return token{kind: tBlob, pos: start, end: i + 1}
	}
	for i < len(l.text) && l.text[i] != '\'' {
		i++
	}
	if i < len(l.text) {
		i++
	}
	return token{kind: tIllegal, pos: start, end: i}
}

// number reads a number as SQLite does: digits, a point and digits, an
// exponent; or a hex number; with digit separators, _, anywhere among the
// digits. A number followed straight away by a character that could be in
// a bare name, such as 12abc or 1e, is a token SQLite doesn't know.
func (l *lexer) number(start int) token {
	t := token{kind: tInt, pos: start}
	i := start
	digits := func(ok func(byte) bool) {
		for {
			c := l.at(i)
			if c == '_' {
				t.kind = tSeparated
			} else if !ok(c) {
				return
			}
			i++
		}
	}
	if l.at(i) == '0' && (l.at(i+1) == 'x' || l.at(i+1) == 'X') && isHex(l.at(i+2)) {
		i += 2
		t.kind = tHex
		digits(isHex)
		if t.kind == tSeparated {
			t.kind = tHex // a hex number with separators is outside the subset as a hex number
		}
	} else {
		digits(isDigit)
		if l.at(i) == '.' {
			if t.kind == tInt {
				t.kind = tReal
			}
			i++
			digits(isDigit)
		}
		if c := l.at(i); (c == 'e' || c == 'E') &&
			(isDigit(l.at(i+1)) || (l.at(i+1) == '+' || l.at(i+1) == '-') && isDigit(l.at(i+2))) {
			if t.kind == tInt {
				t.kind = tReal
			}
			i += 2
			digits(isDigit)
		}
	}
	if isIDChar(l.at(i)) {
		t.kind = tIllegal
		for isIDChar(l.at(i)) {
			i++
		}
	}
	t.end = i
	return t
}

// named reads SQLite's named parameters, :name, @name, $name and #name,
// with Tcl's forms, $name(...) and $a::b, as sqlite3GetToken has them. One
// without a name is a token SQLite doesn't know.
func (l *lexer) named(start int) token {
	t := token{kind: tNamed, pos: start}
	n := 0
	i := start + 1
	for ; i < len(l.text); i++ {
		c := l.text[i]
		if isIDChar(c) {
			n++
		} else if c == '(' && n > 0 {
			i++
			for i < len(l.text) && !isSpace(l.text[i]) && l.text[i] != ')' {
				i++
			}
			if l.at(i) == ')' {
				i++
			} else {
				t.kind = tIllegal
			}
			break
		} else if c == ':' && l.at(i+1) == ':' {
			i++
		} else {
			break
		}
	}
	if n == 0 {
		t.kind = tIllegal
	}
	t.end = i
	return t
}

// unquote returns what a quoted token stands for: its text without the
// quotes, with each doubled quote made one.
func unquote(s string) string {
	q := s[:1]
	s = s[1 : len(s)-1]
	if strings.Contains(s, q) {
		s = strings.ReplaceAll(s, q+q, q)
	}
	return s
}

// unhex returns the bytes of a blob literal's hex digits.
func unhex(s string) string {
	h := s[2 : len(s)-1]
	b := make([]byte, len(h)/2)
	for i := range b {
		b[i] = hexVal(h[2*i])<<4 | hexVal(h[2*i+1])
	}
	return string(b)
}

func hexVal(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10
	case c >= 'A':
		return c - 'A' + 10
	}
	return c - '0'
}

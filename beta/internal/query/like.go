// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package query

import "github.com/hypercrux/hypercrux/beta/internal/value"

// LIKE, as SQLite's likeFunc and patternCompare work it out (SQL.md,
// "LIKE"). Both sides are read as text, with numbers written as text and
// bytes taken as they are. Characters are UTF-8 as sqlite3Utf8Read reads
// them, and matching stops at a NUL byte, since SQLite reads both sides as
// C strings.

// maxLikePattern is SQLITE_MAX_LIKE_PATTERN_LENGTH.
const maxLikePattern = 50000

// likeTruth is likeFunc on the pattern p, the text x and the escape esc,
// when hasEsc: 1 when x matches, 0 when it doesn't, 2 for NULL. A pattern
// over 50,000 bytes is an error, and so is an escape that isn't one
// character, both found before a NULL x or p counts.
func likeTruth(e *Like, p, x, esc value.Value, hasEsc bool) (int, error) {
	if textBytes(p) > maxLikePattern {
		return 0, fail(e, "LIKE or GLOB pattern too complex")
	}
	all, one := uint32('%'), uint32('_')
	var escape uint32
	if hasEsc {
		es, ok := textOf(esc)
		if !ok {
			return 2, nil
		}
		if charsBeforeNUL(es) != 1 {
			return 0, fail(e, "ESCAPE expression must be a single character")
		}
		escape, _ = utf8Read(es, 0)
		if escape == all {
			all = 0
		}
		if escape == one {
			one = 0
		}
	}
	ps, ok := textOf(p)
	if !ok {
		return 2, nil
	}
	xs, ok := textOf(x)
	if !ok {
		return 2, nil
	}
	m := matcher{all: all, one: one, esc: escape}
	if m.compare(ps, 0, xs, 0) == likeMatch {
		return 1, nil
	}
	return 0, nil
}

// textBytes is sqlite3_value_bytes: the length of a value's text, or 0
// for NULL.
func textBytes(v value.Value) int {
	s, _ := textOf(v)
	return len(s)
}

// byteAt reads s as a C string: 0 past its end.
func byteAt(s string, i int) byte {
	if i < len(s) {
		return s[i]
	}
	return 0
}

// utf8First is sqlite3Utf8Trans1: what a lead byte from 0xc0 up holds of
// its character's value.
var utf8First = func() (t [64]byte) {
	for i := range t {
		c := byte(0xc0 + i)
		switch {
		case c < 0xe0:
			t[i] = c & 0x1f
		case c < 0xf0:
			t[i] = c & 0x0f
		case c < 0xf8:
			t[i] = c & 0x07
		case c < 0xfc:
			t[i] = c & 0x03
		case c < 0xfe:
			t[i] = c & 0x01
		}
	}
	return t
}()

// utf8Read is sqlite3Utf8Read: the character at i and the byte after it. A
// lead byte from 0xc0 up takes every continuation byte after it; a
// character that encodes a value below 0x80, a surrogate, 0xfffe or 0xffff
// reads as 0xfffd; and a continuation byte on its own reads as itself.
func utf8Read(s string, i int) (uint32, int) {
	c := uint32(byteAt(s, i))
	i++
	if c >= 0xc0 {
		c = uint32(utf8First[c-0xc0])
		for i < len(s) && s[i]&0xc0 == 0x80 {
			c = c<<6 + uint32(s[i]&0x3f)
			i++
		}
		if c < 0x80 || c&0xfffff800 == 0xd800 || c&0xfffffffe == 0xfffe {
			c = 0xfffd
		}
	}
	return c, i
}

// The results of patternCompare.
const (
	likeMatch           = 0
	likeNoMatch         = 1
	likeNoWildcardMatch = 2 // no match, in spite of a % that tried every place
)

// matcher is patternCompare's compareInfo for LIKE, which ignores case,
// with the escape: all is %, and one is _, unless the escape is one of
// them, which then is 0.
type matcher struct {
	all, one, esc uint32
}

func asciiLower(c uint32) uint32 {
	if 'A' <= c && c <= 'Z' {
		return c + 'a' - 'A'
	}
	return c
}

func asciiUpper(c uint32) uint32 {
	if 'a' <= c && c <= 'z' {
		return c - ('a' - 'A')
	}
	return c
}

// equalNoCase is LIKE's test of two characters: the same, or ASCII
// letters that differ only in case.
func equalNoCase(c, c2 uint32) bool {
	if c == c2 {
		return true
	}
	if plant == "query/like-folds-unicode" && c >= 0x80 && c2 >= 0x80 {
		return c|0x20 == c2|0x20
	}
	return c < 0x80 && c2 < 0x80 && asciiLower(c) == asciiLower(c2)
}

// compare is patternCompare: whether the string str from si matches the
// pattern pat from pi.
func (m *matcher) compare(pat string, pi int, str string, si int) int {
	escaped := -1 // the byte after the last escaped character of the pattern
	for {
		c, next := utf8Read(pat, pi)
		if c == 0 {
			break
		}
		pi = next
		if c == m.all {
			// Skip any more % and _ in the pattern, each _ taking one
			// character of the string.
			for {
				c, pi = utf8Read(pat, pi)
				if c != m.all && !(c == m.one && m.one != 0) {
					break
				}
				if c == m.one {
					var c2 uint32
					if c2, si = utf8Read(str, si); c2 == 0 {
						return likeNoWildcardMatch
					}
				}
			}
			if c == 0 {
				return likeMatch
			}
			if c == m.esc {
				if c, pi = utf8Read(pat, pi); c == 0 {
					return likeNoWildcardMatch
				}
			}
			// Look along the string for c, and match the rest of the
			// pattern from each place it's found.
			if c < 0x80 {
				lo, up := byte(asciiLower(c)), byte(asciiUpper(c))
				for {
					for byteAt(str, si) != 0 && byteAt(str, si) != lo && byteAt(str, si) != up {
						si++
					}
					if byteAt(str, si) == 0 {
						break
					}
					si++
					if r := m.compare(pat, pi, str, si); r != likeNoMatch {
						return r
					}
				}
			} else {
				for {
					c2, next := utf8Read(str, si)
					if c2 == 0 {
						break
					}
					si = next
					if c2 != c {
						continue
					}
					if r := m.compare(pat, pi, str, si); r != likeNoMatch {
						return r
					}
				}
			}
			return likeNoWildcardMatch
		}
		if c == m.esc {
			if c, pi = utf8Read(pat, pi); c == 0 {
				return likeNoMatch
			}
			escaped = pi
		}
		c2, next := utf8Read(str, si)
		si = next
		if equalNoCase(c, c2) {
			continue
		}
		if c == m.one && pi != escaped && c2 != 0 {
			continue
		}
		return likeNoMatch
	}
	if byteAt(str, si) == 0 {
		return likeMatch
	}
	return likeNoMatch
}

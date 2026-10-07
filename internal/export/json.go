// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

package export

import (
	"errors"
	"fmt"
	"unicode/utf16"
	"unicode/utf8"
)

// A small JSON parser for one line of an export. It keeps numbers as they
// were written, so an integer and a real stay apart, keeps an object's
// members in order, and refuses what encoding/json would let by: a member
// that appears twice, and an escape that is half a surrogate pair.

type kind uint8

const (
	kNull kind = iota
	kBool
	kNumber
	kString
	kArray
	kObject
)

type value struct {
	kind kind
	s    string // a string, a number as written, or true or false
	arr  []value
	obj  []member
}

type member struct {
	name  string
	value value
}

// maxNesting is deeper than any export line goes: a line, its fields, and
// a field's value.
const maxNesting = 4

type parser struct {
	s string
	i int
}

// parseLine parses a line that holds one JSON object.
func parseLine(s string) (value, error) {
	p := parser{s: s}
	p.space()
	if p.peek() != '{' {
		return value{}, errors.New("a line holds one JSON object")
	}
	v, err := p.value(0)
	if err != nil {
		return value{}, err
	}
	p.space()
	if p.i != len(p.s) {
		return value{}, fmt.Errorf("something follows the JSON object, at byte %d", p.i+1)
	}
	return v, nil
}

func (p *parser) peek() byte {
	if p.i < len(p.s) {
		return p.s[p.i]
	}
	return 0
}

func (p *parser) space() {
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *parser) errorf(format string, args ...any) error {
	return fmt.Errorf("byte %d: %s", p.i+1, fmt.Sprintf(format, args...))
}

func (p *parser) value(depth int) (value, error) {
	if depth >= maxNesting {
		return value{}, p.errorf("the JSON is nested too deeply")
	}
	switch c := p.peek(); {
	case c == '{':
		return p.object(depth)
	case c == '[':
		return p.array(depth)
	case c == '"':
		s, err := p.str()
		return value{kind: kString, s: s}, err
	case c == '-' || c >= '0' && c <= '9':
		return p.number()
	}
	for _, w := range [...]string{"null", "true", "false"} {
		if len(p.s)-p.i >= len(w) && p.s[p.i:p.i+len(w)] == w {
			p.i += len(w)
			if w == "null" {
				return value{kind: kNull}, nil
			}
			return value{kind: kBool, s: w}, nil
		}
	}
	if p.i >= len(p.s) {
		return value{}, p.errorf("the line ends where a value should be")
	}
	return value{}, p.errorf("a value can't start with %q", p.s[p.i])
}

func (p *parser) object(depth int) (value, error) {
	p.i++
	v := value{kind: kObject}
	p.space()
	if p.peek() == '}' {
		p.i++
		return v, nil
	}
	for {
		p.space()
		if p.peek() != '"' {
			return value{}, p.errorf("a member's name is a string")
		}
		name, err := p.str()
		if err != nil {
			return value{}, err
		}
		for _, m := range v.obj {
			if m.name == name {
				return value{}, p.errorf("the member %q is there twice", name)
			}
		}
		p.space()
		if p.peek() != ':' {
			return value{}, p.errorf("a colon goes after a member's name")
		}
		p.i++
		p.space()
		val, err := p.value(depth + 1)
		if err != nil {
			return value{}, err
		}
		v.obj = append(v.obj, member{name, val})
		p.space()
		switch p.peek() {
		case ',':
			p.i++
		case '}':
			p.i++
			return v, nil
		default:
			return value{}, p.errorf("a comma or a closing brace goes after a member")
		}
	}
}

func (p *parser) array(depth int) (value, error) {
	p.i++
	v := value{kind: kArray}
	p.space()
	if p.peek() == ']' {
		p.i++
		return v, nil
	}
	for {
		p.space()
		val, err := p.value(depth + 1)
		if err != nil {
			return value{}, err
		}
		v.arr = append(v.arr, val)
		p.space()
		switch p.peek() {
		case ',':
			p.i++
		case ']':
			p.i++
			return v, nil
		default:
			return value{}, p.errorf("a comma or a closing bracket goes after a list's value")
		}
	}
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// number reads -?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][+-]?[0-9]+)? and keeps it as
// it was written.
func (p *parser) number() (value, error) {
	start := p.i
	if p.peek() == '-' {
		p.i++
	}
	switch c := p.peek(); {
	case c == '0':
		p.i++
	case c >= '1' && c <= '9':
		for isDigit(p.peek()) {
			p.i++
		}
	default:
		return value{}, p.errorf("a number needs a digit")
	}
	if p.peek() == '.' {
		p.i++
		if !isDigit(p.peek()) {
			return value{}, p.errorf("a digit goes after a decimal point")
		}
		for isDigit(p.peek()) {
			p.i++
		}
	}
	if c := p.peek(); c == 'e' || c == 'E' {
		p.i++
		if c := p.peek(); c == '+' || c == '-' {
			p.i++
		}
		if !isDigit(p.peek()) {
			return value{}, p.errorf("an exponent needs a digit")
		}
		for isDigit(p.peek()) {
			p.i++
		}
	}
	return value{kind: kNumber, s: p.s[start:p.i]}, nil
}

// str reads a string. A string without escapes is a slice of the line.
func (p *parser) str() (string, error) {
	p.i++
	start := p.i
	for p.i < len(p.s) {
		switch c := p.s[p.i]; {
		case c == '"':
			p.i++
			return p.s[start : p.i-1], nil
		case c == '\\':
			return p.escaped(start)
		case c < 0x20:
			return "", p.errorf("a control character in a string must be escaped")
		}
		p.i++
	}
	return "", p.errorf("a string runs past the end of the line")
}

func (p *parser) escaped(start int) (string, error) {
	b := []byte(p.s[start:p.i])
	for p.i < len(p.s) {
		c := p.s[p.i]
		switch {
		case c == '"':
			p.i++
			return string(b), nil
		case c < 0x20:
			return "", p.errorf("a control character in a string must be escaped")
		case c != '\\':
			b = append(b, c)
			p.i++
			continue
		}
		p.i++
		e := p.peek()
		p.i++
		switch e {
		case '"', '\\', '/':
			b = append(b, e)
		case 'b':
			b = append(b, '\b')
		case 'f':
			b = append(b, '\f')
		case 'n':
			b = append(b, '\n')
		case 'r':
			b = append(b, '\r')
		case 't':
			b = append(b, '\t')
		case 'u':
			r, ok := p.hex4()
			if !ok {
				return "", p.errorf(`\u needs four hex digits`)
			}
			if utf16.IsSurrogate(r) {
				lo := rune(-1)
				if r < 0xdc00 && len(p.s)-p.i >= 6 && p.s[p.i] == '\\' && p.s[p.i+1] == 'u' {
					p.i += 2
					lo, _ = p.hex4()
				}
				if r = utf16.DecodeRune(r, lo); r == utf8.RuneError {
					return "", p.errorf(`a \u escape is half of a surrogate pair`)
				}
			}
			b = utf8.AppendRune(b, r)
		default:
			return "", p.errorf("unknown escape in a string")
		}
	}
	return "", p.errorf("a string runs past the end of the line")
}

func (p *parser) hex4() (rune, bool) {
	if len(p.s)-p.i < 4 {
		return 0, false
	}
	var r rune
	for _, c := range []byte(p.s[p.i : p.i+4]) {
		switch {
		case c >= '0' && c <= '9':
			r = r<<4 | rune(c-'0')
		case c >= 'a' && c <= 'f':
			r = r<<4 | rune(c-'a'+10)
		case c >= 'A' && c <= 'F':
			r = r<<4 | rune(c-'A'+10)
		default:
			return 0, false
		}
	}
	p.i += 4
	return r, true
}

// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package format

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	val "github.com/hypercrux/hypercrux/beta/internal/value"
)

// FORMAT.md's rules for one change, under "Changes", "Values" and "Names
// and limits". The encoder and the decoder both check every change with
// problem, so they refuse the same ones, with the same words. The rules
// that depend on the state, such as a put going into a table that exists,
// or a vector having its table's size, are the store's.

// FORMAT.md's limits.
const (
	maxTableName = 63
	maxKey       = 1024
	maxFieldName = 64
	maxTypeRunes = 200
	maxVecDims   = 65536          // the most values in a vector, and the largest vector size
	maxCount     = math.MaxUint16 // the most fields a create change or a put carries
	maxBlob      = math.MaxUint32 // the most bytes in a text or bytes value
)

// problem says what's wrong with c by the rules for a change on its own,
// or returns "" when nothing is. A change uses only the fields of Change
// that its Op names, so one that sets any other is refused too, which
// keeps decode(encode(c)) the same as c.
func (c *Change) problem() string {
	if c.Op < CreateTable || c.Op > Drop {
		return "a change of unknown kind " + c.Op.String()
	}
	if why := c.strays(); why != "" {
		return why
	}
	switch c.Op {
	case CreateTable:
		if !isTableName(c.Table) {
			return badTable(c.Table)
		}
		if c.Size < 0 || c.Size > maxVecDims {
			return fmt.Sprintf("a vector size of %d, outside 0 to 65,536", c.Size)
		}
		if len(c.Names) > maxCount {
			return fmt.Sprintf("%d fields, more than the 65,535 a create change holds", len(c.Names))
		}
		vec := false
		for _, name := range c.Names {
			if !isFieldName(name) {
				return badField(name)
			}
			vec = vec || isVec(name)
		}
		if i, j, ok := clash(len(c.Names), func(i int) string { return c.Names[i] }); ok {
			return fmt.Sprintf("the fields %s and %s, which match regardless of case", show(c.Names[i]), show(c.Names[j]))
		}
		if c.Size != 0 && !vec {
			return fmt.Sprintf("a vector size of %d and no vector field", c.Size)
		}
	case Put:
		if !isKey(c.Key) {
			return badKey(c.Key)
		}
		if len(c.Fields) > maxCount {
			return fmt.Sprintf("%d fields, more than the 65,535 a put holds", len(c.Fields))
		}
		upper := false
		for i, f := range c.Fields {
			if !isFieldName(f.Name) {
				return badField(f.Name)
			}
			if i > 0 && f.Name <= c.Fields[i-1].Name {
				return fmt.Sprintf("the field %s after %s, out of byte order", show(f.Name), show(c.Fields[i-1].Name))
			}
			if why := valueProblem(f.Name, f.Value); why != "" {
				return why
			}
			upper = upper || hasUpper(f.Name)
		}
		// In byte order no two names are the same, so two can only match
		// regardless of case when one has a capital letter.
		if upper {
			if i, j, ok := clash(len(c.Fields), func(i int) string { return c.Fields[i].Name }); ok {
				return fmt.Sprintf("the fields %s and %s, which match regardless of case", show(c.Fields[i].Name), show(c.Fields[j].Name))
			}
		}
	case Delete:
		if !isKey(c.Key) {
			return badKey(c.Key)
		}
	case Link, Unlink:
		switch {
		case !isKey(c.Key):
			return badKey(c.Key)
		case !isLinkType(c.Type):
			return "the link type " + show(c.Type) + ", which breaks the rules for link types"
		case !isKey(c.To):
			return badKey(c.To)
		}
	case Drop:
		if !isTableName(c.Table) {
			return badTable(c.Table)
		}
	}
	return ""
}

// strays names the fields of Change that c sets and its Op doesn't use.
func (c *Change) strays() string {
	var set []string
	if c.Table != "" && c.Op != CreateTable && c.Op != Drop {
		set = append(set, "Table")
	}
	if c.Size != 0 && c.Op != CreateTable {
		set = append(set, "Size")
	}
	if len(c.Names) != 0 && c.Op != CreateTable {
		set = append(set, "Names")
	}
	if c.Key != "" && (c.Op == CreateTable || c.Op == Drop) {
		set = append(set, "Key")
	}
	if len(c.Fields) != 0 && c.Op != Put {
		set = append(set, "Fields")
	}
	if c.Type != "" && c.Op != Link && c.Op != Unlink {
		set = append(set, "Type")
	}
	if c.To != "" && c.Op != Link && c.Op != Unlink {
		set = append(set, "To")
	}
	if set == nil {
		return ""
	}
	return fmt.Sprintf("a %s change that sets %s, which it doesn't use", c.Op, strings.Join(set, " and "))
}

// valueProblem says what's wrong with the value v in the field called
// name, or returns "".
func valueProblem(name string, v val.Value) string {
	vec := isVec(name)
	switch k := v.Kind(); {
	case k == val.KindNull:
		return ""
	case vec && k != val.KindVector:
		return fmt.Sprintf("the vector field %s holding a value of kind %s", show(name), k)
	case !vec && k == val.KindVector:
		return fmt.Sprintf("the field %s holding a vector, which only the vector field holds", show(name))
	}
	switch v.Kind() {
	case val.KindReal:
		if !finite64(math.Float64bits(v.Real())) {
			return fmt.Sprintf("the field %s holding the real number %v", show(name), v.Real())
		}
	case val.KindText:
		if len(v.Raw()) > maxBlob {
			return fmt.Sprintf("the field %s holding %d bytes of text, more than 4,294,967,295", show(name), len(v.Raw()))
		}
		if !utf8.ValidString(v.Raw()) {
			return fmt.Sprintf("the field %s holding text that isn't valid UTF-8", show(name))
		}
	case val.KindBytes:
		if len(v.Raw()) > maxBlob {
			return fmt.Sprintf("the field %s holding %d bytes, more than 4,294,967,295", show(name), len(v.Raw()))
		}
	case val.KindVector:
		return vectorProblem(v.Raw())
	}
	return ""
}

// vectorProblem checks a vector's values, given as their bits, 4 bytes a
// value, little-endian.
func vectorProblem(bits string) string {
	n := len(bits) / 4
	if n < 1 || n > maxVecDims {
		return fmt.Sprintf("a vector of %d values, outside 1 to 65,536", n)
	}
	var or uint32
	for i := 0; i+4 <= len(bits); i += 4 {
		x := uint32(bits[i]) | uint32(bits[i+1])<<8 | uint32(bits[i+2])<<16 | uint32(bits[i+3])<<24
		if x&0x7f800000 == 0x7f800000 {
			return fmt.Sprintf("a vector whose value %d is %v", i/4, math.Float32frombits(x))
		}
		or |= x
	}
	if or&0x7fffffff == 0 {
		return "a vector whose values are all zero"
	}
	return ""
}

// finite64 reports whether a float64's bits make a finite number: neither
// NaN nor infinite.
func finite64(bits uint64) bool { return bits&0x7ff0000000000000 != 0x7ff0000000000000 }

// The rules for names, which are 0.x's.

// isTableName: lower-case letters, digits and underscores, starting with a
// letter, 1 to 63 bytes, and not starting with hc_ or sqlite_.
func isTableName(s string) bool {
	if len(s) < 1 || len(s) > maxTableName || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if c := s[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return !strings.HasPrefix(s, "hc_") && !strings.HasPrefix(s, "sqlite_")
}

// isKey: a table name, a colon, and at least one more byte, in UTF-8
// without zero bytes, 3 to 1,024 bytes in all.
func isKey(s string) bool {
	if len(s) < 3 || len(s) > maxKey {
		return false
	}
	i := strings.IndexByte(s, ':')
	if i < 1 || i == len(s)-1 || !isTableName(s[:i]) {
		return false
	}
	rest := s[i+1:]
	return strings.IndexByte(rest, 0) < 0 && utf8.ValidString(rest)
}

// isFieldName: letters, digits and underscores, starting with a letter or
// an underscore, 1 to 64 bytes, and not key, rowid, oid or _rowid_ in any
// case.
func isFieldName(s string) bool {
	if len(s) < 1 || len(s) > maxFieldName {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', c == '_':
		case '0' <= c && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return !foldEqual(s, "key") && !foldEqual(s, "rowid") && !foldEqual(s, "oid") && !foldEqual(s, "_rowid_")
}

// isLinkType: UTF-8 that doesn't start with a zero byte, 1 to 200 code
// points.
func isLinkType(s string) bool {
	return len(s) >= 1 && len(s) <= 4*maxTypeRunes && s[0] != 0 &&
		utf8.ValidString(s) && utf8.RuneCountInString(s) <= maxTypeRunes
}

// isVec reports whether a field is the vector field: its name matches vec
// regardless of case.
func isVec(name string) bool { return foldEqual(name, "vec") }

// hasUpper reports whether s holds a capital letter.
func hasUpper(s string) bool {
	for i := 0; i < len(s); i++ {
		if 'A' <= s[i] && s[i] <= 'Z' {
			return true
		}
	}
	return false
}

// foldEqual reports whether a and b match regardless of case, for ASCII,
// which is all a field name holds.
func foldEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// clash finds two of n names, given by name, that match regardless of
// case: i before j, and true, or false when no two match. A few names are
// compared pair by pair; more go through a map.
func clash(n int, name func(int) string) (i, j int, ok bool) {
	if n <= 16 {
		for j := 1; j < n; j++ {
			for i := 0; i < j; i++ {
				if foldEqual(name(i), name(j)) {
					return i, j, true
				}
			}
		}
		return 0, 0, false
	}
	seen := make(map[string]int, n)
	for j := 0; j < n; j++ {
		lower := strings.ToLower(name(j)) // the name itself when it has no capitals
		if i, ok := seen[lower]; ok {
			return i, j, true
		}
		seen[lower] = j
	}
	return 0, 0, false
}

// The words for a name that breaks its rules.

func badTable(s string) string {
	return "the table name " + show(s) + ", which breaks the rules for table names"
}

func badKey(s string) string {
	return "the key " + show(s) + ", which breaks the rules for keys"
}

func badField(s string) string {
	return "the field name " + show(s) + ", which breaks the rules for field names"
}

// show quotes s for an error message, cut short when it's long.
func show(s string) string {
	if len(s) <= 40 {
		return strconv.Quote(s)
	}
	return fmt.Sprintf("%s and %d bytes more", strconv.Quote(s[:40]), len(s)-40)
}

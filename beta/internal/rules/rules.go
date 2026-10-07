// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package rules

import (
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/hypercrux/hypercrux/beta/internal/errs"
)

// The limits. All but MaxFields are 0.x's, with 0.x's values.
const (
	// MaxKeyLen is the longest key, in bytes.
	MaxKeyLen = 1024
	// MaxTableLen is the longest table name, in bytes.
	MaxTableLen = 63
	// MaxFieldLen is the longest field name, in bytes.
	MaxFieldLen = 64
	// MaxLinkType is the longest link type, in characters (code points).
	MaxLinkType = 200
	// MaxDims is the most values a vector holds.
	MaxDims = 65536
	// MaxFields is the most fields a put lets a table hold, besides its
	// key. It's the limit 0.x has from SQLite, whose tables hold at most
	// 2,000 columns, the key among them, so every table moves between 0.x
	// and the Beta both ways.
	MaxFields = 1999
	// FormatMaxFields is the most fields FORMAT.md lets a table have, the
	// most a create change can carry. A change list read from a file is
	// held to this one, so MaxFields can rise later without a change to
	// the format, and a build with the lower limit still opens the file.
	FormatMaxFields = 65535
)

// Table checks a table's name: lower-case letters, digits and underscores,
// starting with a letter, 1 to 63 bytes, and not starting with hc_ or
// sqlite_, the prefixes of 0.x's own tables and SQLite's.
func Table(name string) error {
	if !isTableName(name) || strings.HasPrefix(name, "hc_") || strings.HasPrefix(name, "sqlite_") {
		return fmt.Errorf("%w: table name %q: use lower-case letters, digits and underscores, starting with a letter", errs.ErrInvalid, name)
	}
	return nil
}

func isTableName(s string) bool {
	if len(s) == 0 || len(s) > MaxTableLen || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		if c := s[i]; !('a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

// TableOf checks a whole key and returns its table: the part before the
// first colon. A key is a table's name, a colon and at least one more byte,
// 1,024 bytes at most, in valid UTF-8 without zero bytes. The checks and
// their messages are 0.x's, in 0.x's order.
func TableOf(key string) (string, error) {
	i := strings.IndexByte(key, ':')
	if i < 0 {
		return "", fmt.Errorf("%w: key %q has no table: write it as table:id, such as docs:7", errs.ErrInvalid, key)
	}
	t := key[:i]
	if err := Table(t); err != nil {
		return "", fmt.Errorf("key %q: %w", key, err)
	}
	switch {
	case i == len(key)-1:
		return "", fmt.Errorf("%w: key %q has nothing after the colon", errs.ErrInvalid, key)
	case len(key) > MaxKeyLen:
		return "", fmt.Errorf("%w: key is %d bytes, more than %d", errs.ErrInvalid, len(key), MaxKeyLen)
	case !utf8.ValidString(key) || strings.IndexByte(key, 0) >= 0:
		return "", fmt.Errorf("%w: key %q isn't clean UTF-8 text", errs.ErrInvalid, key)
	}
	return t, nil
}

// Field checks a field's name: letters, digits and underscores, starting
// with a letter or an underscore, 1 to 64 bytes, and none of key, rowid,
// oid and _rowid_ in any case. A valid name is ASCII, so its case folds
// as Fold folds it.
func Field(name string) error {
	if !isFieldName(name) || SameName(name, "key") || SameName(name, "rowid") || SameName(name, "oid") || SameName(name, "_rowid_") {
		return fmt.Errorf("%w: field name %q: use letters, digits and underscores, starting with a letter, and not key or rowid", errs.ErrInvalid, name)
	}
	return nil
}

func isFieldName(s string) bool {
	if len(s) == 0 || len(s) > MaxFieldLen || !(isLetter(s[0]) || s[0] == '_') {
		return false
	}
	for i := 1; i < len(s); i++ {
		if c := s[i]; !(isLetter(c) || '0' <= c && c <= '9' || c == '_') {
			return false
		}
	}
	return true
}

func isLetter(c byte) bool { return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' }

// CaseClash is 0.x's error for a put that names one field in two
// spellings, such as a and A. name is one of the two.
func CaseClash(name string) error {
	return fmt.Errorf("%w: fields %q and another differ only in case", errs.ErrInvalid, name)
}

// IsVec reports whether name is the vector field's: vec, in any case.
func IsVec(name string) bool { return SameName(name, "vec") }

// Fold returns name with its ASCII letters in lower case, which is how
// field names match: regardless of ASCII case, as SQLite matches names.
// Other characters stay as they are, so a name with the Kelvin sign in it
// never matches one with a k. It allocates only when there's an upper-case
// letter to change.
func Fold(name string) string {
	for i := 0; i < len(name); i++ {
		if c := name[i]; 'A' <= c && c <= 'Z' {
			b := []byte(name)
			for j := i; j < len(b); j++ {
				if c := b[j]; 'A' <= c && c <= 'Z' {
					b[j] = c + 'a' - 'A'
				}
			}
			return string(b)
		}
	}
	return name
}

// SameName reports whether two names match regardless of ASCII case, as
// Fold(a) == Fold(b) does, without allocating.
func SameName(a, b string) bool {
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

// LinkType checks a link's type: 1 to 200 characters of valid UTF-8, not
// starting with a zero byte. 0.x's Link refuses a type that starts with a
// zero byte through its link table's trigger, since SQLite counts the
// characters of text only up to its first zero byte, and gives a plain
// error there; here it's ErrInvalid, as every other broken rule is.
func LinkType(typ string) error {
	if typ == "" || utf8.RuneCountInString(typ) > MaxLinkType || !utf8.ValidString(typ) {
		return fmt.Errorf("%w: link type %q: use 1 to 200 characters", errs.ErrInvalid, typ)
	}
	if typ[0] == 0 {
		return fmt.Errorf("%w: link type %q starts with a zero byte", errs.ErrInvalid, typ)
	}
	return nil
}

// Vector checks a vector: 1 to 65,536 values, each finite, and not all of
// them zero, with -0 counting as zero. The checks, their order and their
// messages are 0.x's.
func Vector(v []float32) error {
	if len(v) == 0 || len(v) > MaxDims {
		return fmt.Errorf("%w: a vector has 1 to %d values, not %d", errs.ErrInvalid, MaxDims, len(v))
	}
	zero := true
	for i, x := range v {
		if f := float64(x); math.IsNaN(f) || math.IsInf(f, 0) {
			return fmt.Errorf("%w: vector value %d is %v", errs.ErrInvalid, i, x)
		}
		if x != 0 {
			zero = false
		}
	}
	if zero {
		return fmt.Errorf("%w: a vector of only zeros has no direction to compare", errs.ErrInvalid)
	}
	return nil
}

// VectorElsewhere is 0.x's error for a vector given for a field other than
// the vector field.
func VectorElsewhere(field string) error {
	return fmt.Errorf("%w: field %s holds a vector; a record's vector goes in the field vec", errs.ErrInvalid, field)
}

// Text checks text stored in a field: it must be valid UTF-8, as in 0.x.
func Text(field, s string) error {
	if !utf8.ValidString(s) {
		return fmt.Errorf("%w: field %s isn't valid UTF-8; store bytes as []byte", errs.ErrInvalid, field)
	}
	return nil
}

// Real checks a real number stored in a field: it can't be NaN or
// infinite, as in 0.x.
func Real(field string, f float64) error {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return fmt.Errorf("%w: field %s is %v", errs.ErrInvalid, field, f)
	}
	return nil
}

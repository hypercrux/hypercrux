// Copyright HyperCrux.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package value

import (
	"math"
	"math/bits"
	"strings"
)

// SQLite's numbers as text and text as numbers, for SQL (Q1). Each routine
// is SQLite 3.53.4's, as go-sqlite3 v1.14.52's sqlite3-binding.c has it,
// ported step for step, since SQL compares the bits of reals with 0.x's:
//
//   - ParseReal is sqlite3AtoF, which reads a real from text;
//   - ParseInt is sqlite3Atoi64, which reads a whole number from text;
//   - RealText is how SQLite writes a real as text, vdbeMemRenderNum with
//     "%!.17g", from sqlite3FpDecode and the etGENERIC case of
//     sqlite3_str_vappendf;
//   - RealPlaces is "%!.*f", which round() writes and reads back.
//
// They share SQLite's decimal arithmetic: a 64-bit mantissa, its table of
// powers of ten, and the conversions between the two bases that SQLite takes
// from Russ Cox's fpfmt. Go's strconv gives other answers in the cases that
// matter here: its shortest form differs from RealText for about half of
// random reals, and it reads text with more than 19 significant digits to
// the nearest real, where ParseReal keeps only about 19 of them.

// The results of ParseReal, as sqlite3AtoF gives them. The value is
// positive when all of the text reads as a number, apart from spaces at
// either end, and zero or negative when it doesn't; then its low bits still
// say what the longest start of the text that reads was like.
const (
	RealPrefix = 1 // some start of the text reads as a number
	RealPoint  = 2 // the number has a decimal point or an exponent
	RealZero   = 4 // the number is exactly zero, which a value too small for a real isn't
	RealLong   = 8 // the number has more significant digits than the mantissa keeps
)

// The range of powers of ten SQLite's conversions work with.
const (
	powersOf10First = -348
	powersOf10Last  = 347
)

// The most significant 64 bits of 10^p, for p from 0 to 26, and those of
// 10^(27g) for g from -13 to 12, with the next 32 bits in scaleLo. They're
// SQLite's aBase, aScale and aScaleLo, made by its tool/mkfptab.c.
var (
	powBase = [27]uint64{
		0x8000000000000000, 0xa000000000000000, 0xc800000000000000, 0xfa00000000000000,
		0x9c40000000000000, 0xc350000000000000, 0xf424000000000000, 0x9896800000000000,
		0xbebc200000000000, 0xee6b280000000000, 0x9502f90000000000, 0xba43b74000000000,
		0xe8d4a51000000000, 0x9184e72a00000000, 0xb5e620f480000000, 0xe35fa931a0000000,
		0x8e1bc9bf04000000, 0xb1a2bc2ec5000000, 0xde0b6b3a76400000, 0x8ac7230489e80000,
		0xad78ebc5ac620000, 0xd8d726b7177a8000, 0x878678326eac9000, 0xa968163f0a57b400,
		0xd3c21bcecceda100, 0x84595161401484a0, 0xa56fa5b99019a5c8,
	}
	powScale = [26]uint64{
		0x8049a4ac0c5811ae, 0xcf42894a5dce35ea, 0xa76c582338ed2621, 0x873e4f75e2224e68,
		0xda7f5bf590966848, 0xb080392cc4349dec, 0x8e938662882af53e, 0xe65829b3046b0afa,
		0xba121a4650e4ddeb, 0x964e858c91ba2655, 0xf2d56790ab41c2a2, 0xc428d05aa4751e4c,
		0x9e74d1b791e07e48, 0xcccccccccccccccc, 0xcecb8f27f4200f3a, 0xa70c3c40a64e6c51,
		0x86f0ac99b4e8dafd, 0xda01ee641a708de9, 0xb01ae745b101e9e4, 0x8e41ade9fbebc27d,
		0xe5d3ef282a242e81, 0xb9a74a0637ce2ee1, 0x95f83d0a1fb69cd9, 0xf24a01a73cf2dccf,
		0xc3b8358109e84f07, 0x9e19db92b4e31ba9,
	}
	powScaleLo = [26]uint32{
		0x205b896d, 0x52064cad, 0xaf2af2b8, 0x5a7744a7, 0xaf39a475, 0xbd8d794e, 0x547eb47b,
		0x0cb4a5a3, 0x92f34d62, 0x3a6a07f9, 0xfae27299, 0xaa97e14c, 0x775ea265, 0xcccccccc,
		0x00000000, 0x999090b6, 0x69a028bb, 0xe80e6f48, 0x5ec05dd0, 0x14588f14, 0x8f1668c9,
		0x6d953e2c, 0x4abdaf10, 0xbc633b39, 0x0a862f81, 0x6c07a2c2,
	}
)

// multiply160 is sqlite3Multiply160: A is the 96-bit (a<<32)+aLo, and it
// returns the top 64 bits of A*b, with the 32 bits after them.
func multiply160(a uint64, aLo uint32, b uint64) (uint64, uint32) {
	hi, lo := bits.Mul64(a, b)
	th, tl := bits.Mul64(uint64(aLo), b)
	add := th<<32 | tl>>32 // (aLo*b) >> 32
	lo, carry := bits.Add64(lo, add, 0)
	hi += carry
	return hi, uint32(lo >> 32)
}

// powerOfTen is SQLite's powerOfTen: the most significant 64 bits of 10^p,
// for p from -348 to 347, and the 32 bits after them.
func powerOfTen(p int) (uint64, uint32) {
	var g, n int
	switch {
	case p < 0:
		if p == -1 {
			return powScale[13], powScaleLo[13]
		}
		g = p / 27
		n = p % 27
		if n != 0 {
			g--
			n += 27
		}
	case p < 27:
		return powBase[p], 0
	default:
		g = p / 27
		n = p % 27
	}
	s := powScale[g+13]
	if n == 0 {
		return s, powScaleLo[g+13]
	}
	x, lo := multiply160(s, powScaleLo[g+13], powBase[n])
	if x&(1<<63) == 0 {
		x = x<<1 | uint64(lo>>31&1)
		lo = lo<<1 | 1
	}
	return x, lo
}

// pwr10to2 is floor(log2(10^p)), and pwr2to10 floor(log10(2^p)), as SQLite
// works them out. Go's >> on a negative int rounds down, as C's does.
func pwr10to2(p int) int { return (p * 108853) >> 15 }
func pwr2to10(p int) int { return (p * 78913) >> 18 }

// fp2Convert10 is sqlite3Fp2Convert10: for r = m*2^e, with m's top bit set,
// it returns d and p with r about d*10^p, d having at least n digits.
func fp2Convert10(m uint64, e, n int) (uint64, int) {
	p := n - 1 - pwr2to10(e+63)
	pw, _ := powerOfTen(p)
	h, _ := bits.Mul64(m, pw)
	if n == 18 {
		h >>= uint(-(e + pwr10to2(p) + 2))
		return (h + (h<<1)&2) >> 1, -p
	}
	return h >> uint(-(e + pwr10to2(p) + 1)), -p
}

// fp10Convert2 is sqlite3Fp10Convert2: the real nearest d*10^p, as SQLite
// works it out. d isn't 0.
func fp10Convert2(d uint64, p int) float64 {
	if p < powersOf10First {
		return 0
	}
	if p > powersOf10Last {
		return math.Inf(1)
	}
	b := 64 - bits.LeadingZeros64(d)
	lp := pwr10to2(p)
	e := 53 - b - lp
	if e > 1074 {
		if e >= 1130 {
			return 0
		}
		e = 1074
	}
	s := -(e - (64 - b) + lp + 3)
	pwr10h, pwr10l := powerOfTen(p)
	if pwr10l != 0 {
		pwr10h++
		pwr10l = ^pwr10l
	}
	x := d << uint(64-b)
	hi, lo := bits.Mul64(x, pwr10h)
	mid1 := uint32(lo >> 32)
	sticky := uint64(1)
	if hi&(uint64(1)<<uint(s)-1) == 0 {
		h2, _ := bits.Mul64(x, uint64(pwr10l)<<32)
		mid2 := uint32(h2 >> 32)
		if mid1-mid2 <= 1 {
			sticky = 0
		}
		if mid1 < mid2 {
			hi--
		}
	}
	u := hi>>uint(s) | sticky
	if u >= uint64(1)<<55-2 {
		u = u>>1 | u&1
		e--
	}
	m := (u + 1 + (u>>2)&1) >> 2
	if e <= -972 {
		return math.Inf(1)
	}
	if m&(uint64(1)<<52) != 0 {
		m = m&^(uint64(1)<<52) | uint64(1075-e)<<52
	}
	return math.Float64frombits(m)
}

// isSpace is sqlite3Isspace: the space, tab, line feed, vertical tab, form
// feed and carriage return.
func isSpace(c byte) bool { return c == ' ' || '\t' <= c && c <= '\r' }

func isDigit(c byte) bool { return '0' <= c && c <= '9' }

// ParseReal reads a real from text as sqlite3AtoF does, and gives the real
// with a result made of the Real bits above: positive when all of s reads
// as a number, apart from spaces at either end, and zero or negative
// otherwise. A number is an optional sign, digits with an optional point
// and fraction or a point and digits, then an optional exponent, after any
// spaces. When nothing reads, the real is 0 and the result is 0; otherwise
// the real is the longest start that reads.
//
// The text ends at its first NUL byte, as SQLite reads it as a C string.
// Only about the first 19 significant digits count, as in SQLite, so
// "3500000000000000.2500001" gives 3500000000000000.0, where Go's
// strconv.ParseFloat rounds it up.
func ParseReal(s string) (float64, int) {
	if i := strings.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	at := func(i int) byte {
		if i < len(s) {
			return s[i]
		}
		return 0
	}
	const big = (math.MaxUint64 - 9) / 10
	z := 0
	neg := false
	var m uint64 // the mantissa: the value is m * 10^d
	d := 0
	state := 0
	for isSpace(at(z)) {
		z++
	}
	if c := at(z); c == '-' || c == '+' {
		neg = c == '-'
		z++
	}
	if isDigit(at(z)) {
		state = 1
		m = uint64(at(z) - '0')
		z++
		for isDigit(at(z)) {
			m = m*10 + uint64(at(z)-'0')
			z++
			if m >= big {
				state = 9
				for isDigit(at(z)) {
					z++
					d++
				}
				break
			}
		}
	}
	if at(z) == '.' {
		z++
		if isDigit(at(z)) {
			state |= 1
			for {
				if m < big {
					m = m*10 + uint64(at(z)-'0')
					d--
				} else {
					state = 11
				}
				z++
				if !isDigit(at(z)) {
					break
				}
			}
		} else if state == 0 {
			return 0, 0
		}
		state |= 2
	} else if state == 0 {
		return 0, 0
	}
	if c := at(z); c == 'e' || c == 'E' {
		z++
		sign := 1
		if at(z) == '-' {
			sign = -1
			z++
		} else if at(z) == '+' {
			z++
		}
		if isDigit(at(z)) {
			exp := int(at(z) - '0')
			z++
			state |= 2
			for isDigit(at(z)) {
				if exp < 10000 {
					exp = exp*10 + int(at(z)-'0')
				} else {
					exp = 10000
				}
				z++
			}
			d += sign * exp
		} else {
			z-- // left at the e, or at its sign, so the result says it didn't all read
		}
	}
	var r float64
	if m == 0 {
		state |= 4
	} else {
		r = fp10Convert2(m, d)
	}
	if neg {
		r = -r
	}
	for isSpace(at(z)) {
		z++
	}
	if z >= len(s) {
		return r, state
	}
	return r, int(int32(0xfffffff0 | uint32(state)))
}

// ParseInt reads a whole number from text as sqlite3Atoi64 does, all of s,
// NUL bytes included. It skips spaces and an optional sign, and reads the
// digits that follow. A number past 64 bits is clamped to the largest or
// the smallest. The result is SQLite's:
//
//	-1  not even a start of s reads as a whole number, and the number is 0
//	 0  it reads, and fits in 64 bits
//	 1  it reads, with something other than spaces after it
//	 2  it's too big for 64 bits
//	 3  it's 9223372036854775808 exactly, which fits only with a minus sign
func ParseInt(s string) (int64, int) {
	z, end := 0, len(s)
	for z < end && isSpace(s[z]) {
		z++
	}
	neg := false
	if z < end {
		switch s[z] {
		case '-':
			neg = true
			z++
		case '+':
			z++
		}
	}
	start := z
	for z < end && s[z] == '0' {
		z++
	}
	var u uint64
	i := 0
	for z+i < end && isDigit(s[z+i]) {
		u = u*10 + uint64(s[z+i]-'0')
		i++
	}
	var n int64
	switch {
	case u > math.MaxInt64:
		n = math.MaxInt64
		if neg {
			n = math.MinInt64
		}
	case neg:
		n = -int64(u)
	default:
		n = int64(u)
	}
	rc := 0
	switch {
	case i == 0 && start == z:
		rc = -1
	case z+i < end:
		for j := z + i; j < end; j++ {
			if !isSpace(s[j]) {
				rc = 1
				break
			}
		}
	}
	if i < 19 {
		return n, rc
	}
	c := 1
	if i == 19 {
		c = compare2pow63(s[z : z+19])
	}
	if c < 0 {
		return n, rc
	}
	n = math.MaxInt64
	if neg {
		n = math.MinInt64
	}
	if c > 0 {
		return n, 2
	}
	if neg {
		return n, rc
	}
	return n, 3
}

// compare2pow63 compares 19 digits with 9223372036854775808, as SQLite's
// compare2pow63 does.
func compare2pow63(z string) int {
	const pow63 = "922337203685477580"
	c := 0
	for i := 0; c == 0 && i < 18; i++ {
		c = (int(z[i]) - int(pow63[i])) * 10
	}
	if c == 0 {
		c = int(z[18]) - '8'
	}
	return c
}

// fpDecoded is SQLite's FpDecode: a real as decimal digits.
type fpDecoded struct {
	sign    byte   // '+' or '-'
	special int    // 1 for an infinity, 2 for NaN, and 0 otherwise
	digits  []byte // the significant digits, without the zeros that end them
	dp      int    // where the decimal point goes, counting from the first digit
}

// fpDecode is sqlite3FpDecode: r as decimal digits, rounded to -round
// places after the point when round is 0 or less, or to round significant
// digits otherwise, and to at most most digits in all.
func fpDecode(r float64, round, most int) fpDecoded {
	var p fpDecoded
	switch {
	case r < 0:
		p.sign = '-'
		r = -r
	case r == 0:
		p.sign = '+'
		p.digits = []byte{'0'}
		p.dp = 1
		return p
	default:
		p.sign = '+'
	}
	v := math.Float64bits(r)
	e := int(v>>52) & 0x7ff
	if e == 0x7ff {
		p.special = 1
		if v != 0x7ff0000000000000 {
			p.special = 2
		}
		return p
	}
	v &= 0x000fffffffffffff
	if e == 0 {
		nn := bits.LeadingZeros64(v)
		v <<= uint(nn)
		e = -1074 - nn
	} else {
		v = v<<11 | 1<<63
		e -= 1086
	}
	n := 18
	if round > 0 && round < 18 {
		n = round + 1
	}
	v, exp := fp2Convert10(v, e, n)

	// The digits go at the end of buf, as SQLite writes them into zBuf,
	// which leaves room in front for a carry or a leading zero.
	var buf [21]byte
	i := 20
	for v >= 10 {
		kk := v % 100
		buf[i-2] = byte('0' + kk/10)
		buf[i-1] = byte('0' + kk%10)
		i -= 2
		v /= 100
	}
	if v != 0 {
		i--
		buf[i] = byte('0' + v)
	}
	n = 20 - i
	p.dp = n + exp
	if round <= 0 {
		round = p.dp - round
		if round == 0 && buf[i] >= '5' {
			round = 1
			i--
			buf[i] = '0'
			n++
			p.dp++
		}
	}
	z := buf[i:]
	if round > 0 && (round < n || n > most) {
		if round > most {
			round = most
		}
		if round == 17 {
			// For 17 digits, which only "%!.17g" asks for, SQLite tries a
			// shorter form that reads back to the same real, so that 49.47
			// gives 49.47 and not 49.469999999999999.
			if z[15] == '9' && z[14] == '9' {
				jj := 14
				for jj > 0 && z[jj-1] == '9' {
					jj--
				}
				var v2 uint64
				if jj == 0 {
					v2 = 1
				} else {
					v2 = uint64(z[0] - '0')
					for kk := 1; kk < jj; kk++ {
						v2 = v2*10 + uint64(z[kk]-'0')
					}
					v2++
				}
				if r == fp10Convert2(v2, exp+n-jj) {
					round = jj + 1
				}
			} else if p.dp >= n || z[15] == '0' && z[14] == '0' && z[13] == '0' {
				jj := 13
				for z[jj-1] == '0' {
					jj--
				}
				v2 := uint64(z[0] - '0')
				for kk := 1; kk < jj; kk++ {
					v2 = v2*10 + uint64(z[kk]-'0')
				}
				if r == fp10Convert2(v2, exp+n-jj) {
					round = jj + 1
				}
			}
		}
		n = round
		if z[round] >= '5' {
			for j := round - 1; ; j-- {
				z[j]++
				if z[j] <= '9' {
					break
				}
				z[j] = '0'
				if j == 0 {
					i--
					z = buf[i:]
					z[0] = '1'
					n++
					p.dp++
					break
				}
			}
		}
	}
	for z[n-1] == '0' {
		n--
	}
	p.digits = z[:n:n]
	return p
}

// RealText gives a real as SQL writes it where it needs text: for ||, CAST
// to TEXT, the text functions and LIKE. That's SQLite's "%!.17g":
//
//   - 17 significant digits, or fewer when a shorter form reads back to
//     the same bits, so 0.1 gives 0.1 and 0.1+0.2 gives 0.30000000000000004;
//   - a decimal point even for a whole number, 1.0 and 100.0;
//   - an exponent of two digits or more, 1.0e+21 or 1.0e-07, when the
//     decimal exponent is below -4 or above 16;
//   - 0.0 for both zeros, and Inf and -Inf for the infinities.
//
// ParseReal reads the text back to the same bits.
func RealText(r float64) string { return formatReal(r, 'g', 17) }

// RealPlaces gives a real with places digits after the point, at most, as
// SQLite's "%!.*f" writes it for round(): rounded to that many places,
// without the zeros that would end it, and with at least one digit after
// the point, so 2.5 with 3 places gives 2.5. places is from 0 to 30.
func RealPlaces(r float64, places int) string { return formatReal(r, 'f', places) }

// formatReal is the etFLOAT and etGENERIC cases of sqlite3_str_vappendf
// with the ! flag, which keeps the point and lets up to 20 digits through,
// and no other flag, width or thousands separator.
func formatReal(r float64, kind byte, precision int) string {
	var round int
	if kind == 'f' {
		round = -precision
	} else {
		if precision == 0 {
			precision = 1
		}
		round = precision
	}
	s := fpDecode(r, round, 20)
	switch {
	case s.special == 2:
		return "NaN"
	case s.special == 1 && s.sign == '-':
		return "-Inf"
	case s.special == 1:
		return "Inf"
	}
	exp := s.dp - 1
	if kind == 'g' {
		precision--
		if exp < -4 || exp > precision {
			kind = 'e'
		} else {
			precision -= exp
			kind = 'f'
		}
	}
	e2 := s.dp - 1
	if kind == 'e' {
		e2 = 0
	}
	out := make([]byte, 0, 24)
	if s.sign == '-' {
		out = append(out, '-')
	}
	// The digits before the point.
	j := 0
	if e2 < 0 {
		out = append(out, '0')
	} else {
		j = min(e2+1, len(s.digits))
		out = append(out, s.digits[:j]...)
		for e2 -= j; e2 >= 0; e2-- {
			out = append(out, '0')
		}
	}
	out = append(out, '.')
	// Zeros after the point before the first significant digit, then the
	// digits after the point.
	if e2 < -1 && precision > 0 {
		nn := min(-1-e2, precision)
		for k := 0; k < nn; k++ {
			out = append(out, '0')
		}
		precision -= nn
	}
	if precision > 0 {
		if nn := min(len(s.digits)-j, precision); nn > 0 {
			out = append(out, s.digits[j:j+nn]...)
		}
	}
	// The ! flag takes off the zeros that end it, and keeps one digit
	// after the point.
	for out[len(out)-1] == '0' {
		out = out[:len(out)-1]
	}
	if out[len(out)-1] == '.' {
		out = append(out, '0')
	}
	if kind == 'e' {
		out = append(out, 'e')
		if exp < 0 {
			out = append(out, '-')
			exp = -exp
		} else {
			out = append(out, '+')
		}
		if exp >= 100 {
			out = append(out, byte('0'+exp/100))
			exp %= 100
		}
		out = append(out, byte('0'+exp/10), byte('0'+exp%10))
	}
	return string(out)
}

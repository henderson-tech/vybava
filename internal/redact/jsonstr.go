package redact

import (
	"bytes"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"
)

// eachString calls fn with the content bounds (between the quotes) of every
// string literal in a VALID JSON text — keys included. Validity is the
// caller's promise: outside a string, a quote only ever opens one.
func eachString(b []byte, fn func(start, end int)) {
	for i := 0; i < len(b); i++ {
		if b[i] != '"' {
			continue
		}
		j := i + 1
		for j < len(b) && b[j] != '"' {
			if b[j] == '\\' {
				j++
			}
			j++
		}
		fn(i+1, j)
		i = j
	}
}

// unit reads one source unit of a string literal's raw content at r: a plain
// byte, a two-byte escape, a \uXXXX (a surrogate pair is one unit) — and
// returns its raw length and the bytes it decodes to. decode and rawAt both
// walk through here, so decoded offsets and raw offsets can never disagree.
func unit(raw []byte, r int, scratch []byte) (rl int, dec []byte) {
	if raw[r] != '\\' || r+1 >= len(raw) {
		return 1, raw[r : r+1]
	}
	switch raw[r+1] {
	case 'u':
		cp, ok := hex4(raw, r+2)
		if !ok {
			return 2, raw[r : r+2] // malformed: keep the bytes, stay in step
		}
		rl, ch := 6, rune(cp)
		if utf16.IsSurrogate(ch) {
			if lo, ok := hex4(raw, r+8); ok && r+7 < len(raw) && raw[r+6] == '\\' && raw[r+7] == 'u' {
				if pair := utf16.DecodeRune(ch, rune(lo)); pair != utf8.RuneError {
					ch, rl = pair, 12
				}
			}
			if rl == 6 {
				ch = utf8.RuneError
			}
		}
		return rl, utf8.AppendRune(scratch[:0], ch)
	case 'b':
		return 2, []byte{'\b'}
	case 'f':
		return 2, []byte{'\f'}
	case 'n':
		return 2, []byte{'\n'}
	case 'r':
		return 2, []byte{'\r'}
	case 't':
		return 2, []byte{'\t'}
	default: // \" \\ \/
		return 2, raw[r+1 : r+2]
	}
}

func hex4(raw []byte, at int) (uint64, bool) {
	if at+4 > len(raw) {
		return 0, false
	}
	v, err := strconv.ParseUint(string(raw[at:at+4]), 16, 32)
	return v, err == nil
}

// decode returns a literal's value; content without a backslash is its own.
func decode(raw []byte) string {
	if bytes.IndexByte(raw, '\\') < 0 {
		return string(raw)
	}
	out := make([]byte, 0, len(raw))
	scratch := make([]byte, 0, 4)
	for r := 0; r < len(raw); {
		rl, dec := unit(raw, r, scratch)
		out = append(out, dec...)
		r += rl
	}
	return string(out)
}

// rawAt maps decoded offset p to its raw offset. A p inside one unit's
// decoded bytes (a multi-byte \u escape) rounds to the unit's start, or past
// its end with up — so a span always covers whole escapes and never splits
// one, which keeps an overwrite JSON-valid.
func rawAt(raw []byte, p int, up bool) int {
	if bytes.IndexByte(raw, '\\') < 0 {
		return p
	}
	scratch := make([]byte, 0, 4)
	d := 0
	for r := 0; r < len(raw); {
		if d >= p {
			return r
		}
		rl, dec := unit(raw, r, scratch)
		if d+len(dec) > p {
			if up {
				return r + rl
			}
			return r
		}
		d += len(dec)
		r += rl
	}
	return len(raw)
}

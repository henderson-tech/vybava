package redact

import (
	"bytes"
	"strconv"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/henderson-tech/vybava/internal/secretscan"
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
// returns its raw length and the bytes it decodes to. decode and rawSpans both
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

// rawSpans maps decoded spans — sorted and non-overlapping, as Find returns
// them — to raw offsets in ONE walk of the literal (a walk per span is
// quadratic on a large escaped tool result with many findings). A start
// inside one unit's decoded bytes (a multi-byte \u escape) rounds to the
// unit's start, an end past the unit's end — so a span always covers whole
// escapes and never splits one, which keeps an overwrite JSON-valid.
func rawSpans(raw []byte, spans []secretscan.Span) [][2]int {
	out := make([][2]int, len(spans))
	if bytes.IndexByte(raw, '\\') < 0 {
		for i, s := range spans {
			out[i] = [2]int{s.Start, s.End}
		}
		return out
	}
	// Targets in order: start0, end0, start1, end1, … — never decreasing.
	n := 2 * len(spans)
	target := func(k int) int {
		if k%2 == 0 {
			return spans[k/2].Start
		}
		return spans[k/2].End
	}
	scratch := make([]byte, 0, 4)
	k, d := 0, 0
	for r := 0; r < len(raw) && k < n; {
		rl, dec := unit(raw, r, scratch)
		for k < n {
			p := target(k)
			switch {
			case p <= d: // on this unit's boundary
				out[k/2][k%2] = r
			case p < d+len(dec): // inside this unit
				out[k/2][k%2] = r + rl*(k%2)
			default:
				goto next
			}
			k++
		}
	next:
		d += len(dec)
		r += rl
	}
	for ; k < n; k++ {
		out[k/2][k%2] = len(raw)
	}
	return out
}

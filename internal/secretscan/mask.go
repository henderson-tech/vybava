package secretscan

import (
	"regexp"
	"slices"
	"strings"
)

// Fill is the same-length replacement for n bytes of secret material:
// `[REDACTED:<detector>]` padded with `*` when it fits, n × `*` when it does
// not. Same length keeps every byte offset after it valid — the contract the
// transcript redactor relies on — and the result is plain ASCII, safe inside a
// JSON string. Find treats a Fill as a placeholder, so a second pass finds 0.
func Fill(detector string, n int) []byte {
	token := "[REDACTED:" + detector + "]"
	if len(token) > n {
		return []byte(strings.Repeat("*", n))
	}
	return []byte(token + strings.Repeat("*", n-len(token)))
}

// Redact returns text with every span replaced by `[REDACTED:<detector>]` —
// the form a message may print when it has to quote the offending line.
func Redact(text string, spans []Span) string {
	var b strings.Builder
	last := 0
	for _, s := range spans {
		b.WriteString(text[last:s.Start])
		b.WriteString("[REDACTED:" + s.Detector + "]")
		last = s.End
	}
	b.WriteString(text[last:])
	return b.String()
}

// Quote renders a line some rule flagged, for a message: only what precedes
// its first detected secret or first ':'/'=', then `[withheld: <detectors>]`.
// The caller's rule may be broader than Find, and a line holding one secret
// may hold another no detector recognises — so nothing after either cut is
// ever quoted. The kept prefix (`+aws_key =`) is what locates the line.
func Quote(line string) string {
	spans := Find(line, All, nil)
	cut := len(line)
	if i := strings.IndexAny(line, ":="); i >= 0 {
		cut = i + 1
	}
	if len(spans) > 0 {
		cut = min(cut, spans[0].Start)
	}
	if cut == len(line) {
		return line
	}
	var detectors []string
	for _, s := range spans {
		if !slices.Contains(detectors, s.Detector) {
			detectors = append(detectors, s.Detector)
		}
	}
	if len(detectors) == 0 {
		return strings.TrimRight(line[:cut], " ") + " [withheld]"
	}
	return strings.TrimRight(line[:cut], " ") + " [withheld: " + strings.Join(detectors, ", ") + "]"
}

var (
	// reWord is a run that could be key material a detector missed — an
	// alphabetic password beside a known value included.
	reWord = regexp.MustCompile(`[A-Za-z0-9+/_.-]+`)
	// reVarName is what Shape keeps: an env-style name (MAIL_PASSWORD).
	reVarName = regexp.MustCompile(`^[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+$`)
)

// Shape renders a finding for a human without its value: a few bytes of
// context either side, the span as ‹detector›, every other word of 3+ as <w>
// unless it is a SCREAMING_SNAKE name in name position (NAME=, "NAME":),
// control characters flattened. The name a leak sits under (MAIL_PASSWORD=)
// stays readable; nothing that could be a value does.
func Shape(text string, s Span) string {
	const before, after = 24, 12
	from, to := max(0, s.Start-before), min(len(text), s.End+after)
	// Never start mid-word: a name cut to `SSWORD=` would read as a value.
	for from > 0 && s.Start-from < before+64 && isIdent(text[from-1]) {
		from--
	}
	// Never cut a UTF-8 sequence.
	for from > 0 && from < len(text) && text[from]&0xC0 == 0x80 {
		from--
	}
	for to < len(text) && text[to]&0xC0 == 0x80 {
		to++
	}
	mask := func(v string) string {
		var b strings.Builder
		last := 0
		for _, w := range reWord.FindAllStringIndex(v, -1) {
			// A name only in name position — NAME= · NAME: · NAME = (bare)
			// · "NAME": (a quoted key, colon right after) — else it is an
			// uppercase value; `"TOP_SECRET" = x` quotes an argument.
			rest := v[w[1]:]
			named := reVarName.MatchString(v[w[0]:w[1]]) &&
				(strings.IndexAny(strings.TrimLeft(rest, " \t"), "=:") == 0 ||
					len(rest) > 1 && (rest[0] == '"' || rest[0] == '\'') && rest[1] == ':')
			if w[1]-w[0] >= 3 && !named {
				b.WriteString(v[last:w[0]])
				b.WriteString("<w>")
				last = w[1]
			}
		}
		b.WriteString(v[last:])
		return strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f {
				return ' '
			}
			return r
		}, b.String())
	}
	out := mask(text[from:s.Start]) + "‹" + s.Detector + "›" + mask(text[s.End:to])
	if from > 0 {
		out = "…" + out
	}
	if to < len(text) {
		out += "…"
	}
	return out
}

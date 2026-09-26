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

// reWordy is a digit-bearing run — possibly key material a
// detector missed; Shape masks it too, so context never carries a value.
var reWordy = regexp.MustCompile(`[A-Za-z0-9+/_=.-]*[0-9][A-Za-z0-9+/_=.-]*`)

// Shape renders a finding for a human without its value: a few bytes of
// context either side, the span as ‹detector›, any other digit-bearing run of
// 8+ — or one the window cuts, whatever its length — as <w>, control
// characters flattened. The names around a leak (MAIL_PASSWORD=) stay
// readable; nothing that could be a value does.
func Shape(text string, s Span) string {
	const before, after = 24, 12
	from, to := max(0, s.Start-before), min(len(text), s.End+after)
	// Never cut a UTF-8 sequence.
	for from > 0 && from < len(text) && text[from]&0xC0 == 0x80 {
		from--
	}
	for to < len(text) && text[to]&0xC0 == 0x80 {
		to++
	}
	mask := func(v string, cutLeft, cutRight bool) string {
		var b strings.Builder
		last := 0
		for _, w := range reWordy.FindAllStringIndex(v, -1) {
			if w[1]-w[0] >= 8 || cutLeft && w[0] == 0 || cutRight && w[1] == len(v) {
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
	out := mask(text[from:s.Start], from > 0, false) + "‹" + s.Detector + "›" + mask(text[s.End:to], false, to < len(text))
	if from > 0 {
		out = "…" + out
	}
	if to < len(text) {
		out += "…"
	}
	return out
}

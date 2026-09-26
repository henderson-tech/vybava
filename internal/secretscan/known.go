package secretscan

import (
	"bufio"
	"bytes"
	"strings"
)

// MinKnown is the shortest out-of-band value matched exactly: anything
// shorter collides with ordinary text too often to redact blind.
const MinKnown = 8

// Known holds exact secret values supplied out of band — vault items onyx
// injects into the process environment, the values of a .env file. They are
// matched verbatim and reported as detector "known"; they are never returned.
// A nil *Known matches nothing.
type Known struct {
	values []string
	seen   map[string]bool
}

// Add registers one value and reports whether it was taken: a placeholder or
// a value shorter than MinKnown is refused.
func (k *Known) Add(v string) bool {
	v = strings.TrimSpace(v)
	if len(v) < MinKnown || placeholder(v) || k.seen[v] {
		return false
	}
	if k.seen == nil {
		k.seen = map[string]bool{}
	}
	k.seen[v] = true
	k.values = append(k.values, v)
	return true
}

// AddDotenv registers the value of every secret-named assignment in a .env
// file (SecretName decides, so APP_URL and MAIL_HOST stay out) and returns
// how many it took.
func (k *Known) AddDotenv(data []byte) int {
	taken := 0
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		line = strings.TrimPrefix(line, "export ")
		name, value, ok := strings.Cut(line, "=")
		if !ok || strings.HasPrefix(line, "#") || !SecretName(strings.TrimSpace(name)) {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		if k.Add(value) {
			taken++
		}
	}
	return taken
}

// Len is the number of values held.
func (k *Known) Len() int {
	if k == nil {
		return 0
	}
	return len(k.values)
}

func (k *Known) find(text string) []Span {
	if k == nil {
		return nil
	}
	var spans []Span
	for _, v := range k.values {
		for from := 0; ; {
			i := strings.Index(text[from:], v)
			if i < 0 {
				break
			}
			spans = append(spans, Span{Start: from + i, End: from + i + len(v), Detector: "known"})
			from += i + len(v)
		}
	}
	return spans
}

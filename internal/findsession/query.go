package findsession

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// A phrase needle is a run of plain words this many bytes long: shorter
	// matches unrelated transcripts, longer leaves Go's vectorised search
	// and spans more markup the terminal rendered away.
	minPhrase = 16
	maxPhrase = 32
	// maxNeedles caps the needles per query: each is one pass over a file.
	maxNeedles = 8
)

// breakers end a run: the terminal renders markdown markers away and the
// transcript stores quotes and backslashes JSON-escaped, so no needle may
// span one.
const breakers = "*`\"\\|\t"

// leaders are the line prefixes a rendered conversation adds or keeps from
// markdown — bullets, tree glyphs, prompt marks, heading hashes.
const leaders = "-*•⏺✔✗⎿└├│>❯›#·"

var numberedItem = regexp.MustCompile(`^\d{1,3}[.)]\s+`)

// Fragments turns a query into literal needles and how many of them a
// transcript must hold to match. A paste yields phrases of plain words
// spread across it, half of which must match: the terminal drops inline
// code and bold markers, so a phrase touching one misses, as does TUI
// chrome. A short phrase yields its words, all of which must match.
func Fragments(query string) (frags []string, need int) {
	var phrases []string
	seen := map[string]bool{}
	for _, line := range strings.Split(query, "\n") {
		for _, run := range splitRuns(stripLeaders(line)) {
			for _, phrase := range plainPhrases(run) {
				if !seen[phrase] {
					seen[phrase] = true
					phrases = append(phrases, phrase)
				}
			}
		}
	}
	if len(phrases) > 0 {
		frags = spread(phrases, maxNeedles)
		return frags, max(1, (len(frags)+1)/2)
	}
	for _, word := range strings.FieldsFunc(query, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(breakers, r)
	}) {
		if utf8.RuneCountInString(word) >= 3 && !seen[word] {
			seen[word] = true
			frags = append(frags, word)
		}
	}
	frags = spread(frags, maxNeedles)
	return frags, len(frags)
}

// plainPhrases cuts a run into consecutive, non-overlapping phrases of
// plain words. A word carrying code punctuation (#, /, ~, = …) was likely
// an inline code span and ends the phrase.
func plainPhrases(run string) []string {
	var out []string
	var words []string
	length := 0
	flush := func() {
		if len(words) >= 3 && length >= minPhrase {
			out = append(out, strings.Join(words, " "))
		}
		words, length = nil, 0
	}
	for _, word := range strings.Fields(run) {
		if !plainWord(word) {
			flush()
			continue
		}
		if length+1+len(word) > maxPhrase {
			flush()
		}
		if length > 0 {
			length++
		}
		words = append(words, word)
		length += len(word)
	}
	flush()
	return out
}

func plainWord(word string) bool {
	for _, r := range word {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune(".,;:!?'’()–-", r) {
			return false
		}
	}
	return true
}

// spread picks n items evenly across a list, keeping its order.
func spread(items []string, n int) []string {
	if len(items) <= n {
		return items
	}
	out := make([]string, 0, n)
	for i := range n {
		out = append(out, items[i*(len(items)-1)/(n-1)])
	}
	return out
}

func stripLeaders(line string) string {
	for {
		trimmed := strings.TrimLeft(strings.TrimSpace(line), leaders)
		trimmed = numberedItem.ReplaceAllString(strings.TrimSpace(trimmed), "")
		if trimmed == line {
			return line
		}
		line = trimmed
	}
}

// splitRuns cuts a line at breakers and at padding (two or more spaces, as
// table cells and alignment produce).
func splitRuns(line string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(line, func(r rune) bool { return strings.ContainsRune(breakers, r) }) {
		for _, run := range strings.Split(part, "  ") {
			if run = strings.TrimSpace(run); run != "" {
				out = append(out, run)
			}
		}
	}
	return out
}

var sessionID = regexp.MustCompile(`^[0-9a-f]{8}(-[0-9a-f]{1,4}){0,3}(-[0-9a-f]{1,12})?$`)

// ParseID reports whether a query is a session id or an id prefix (at least
// the first 8 hex digits), forgiving the punctuation a copied sentence
// carries.
func ParseID(query string) (string, bool) {
	id := strings.ToLower(strings.Trim(strings.TrimSpace(query), ".,;:'\"`()[]<>"))
	return id, sessionID.MatchString(id)
}

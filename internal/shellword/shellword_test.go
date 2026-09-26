package shellword

import "testing"

// TestQuote: a plain word passes through; a space, a glob or an apostrophe
// comes back as one single-quoted word.
func TestQuote(t *testing.T) {
	for in, want := range map[string]string{
		"/Users/me/repo":    "/Users/me/repo",
		"/Users/me/My Repo": "'/Users/me/My Repo'",
		"Done!":             "'Done!'",
		"it's":              `'it'\''s'`,
		"":                  "''",
	} {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %s, want %s", in, got, want)
		}
	}
}

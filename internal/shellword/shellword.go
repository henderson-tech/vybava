// Package shellword renders a string as one POSIX shell word, for the
// commands vybava prints for a human or agent to paste (Fix lines, `next`
// steps, generated scripts). The one shared quoter: reach for it instead of
// another local copy.
package shellword

import "strings"

// Quote returns s unchanged when it holds no shell metacharacter, else
// single-quoted with each ' as '\”. A bare path stays readable; a path with
// a space, glob or history character (`Done!` under zsh) survives the paste.
func Quote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n'\"$`\\{};&|<>()!#*?[]~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

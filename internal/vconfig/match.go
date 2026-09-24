package vconfig

import (
	"path"
	"strings"
)

// MatchPath is the one glob dialect of vybava.config sections (guards.noRead,
// merge.generated, …): slash-separated repository paths, ** spans zero or
// more complete path components, ordinary components use Go's glob syntax.
func MatchPath(pattern, name string) bool {
	parts, names := strings.Split(pattern, "/"), strings.Split(name, "/")
	var match func(int, int) bool
	match = func(i, j int) bool {
		if i == len(parts) {
			return j == len(names)
		}
		if parts[i] == "**" {
			return match(i+1, j) || (j < len(names) && match(i, j+1))
		}
		if j == len(names) {
			return false
		}
		ok, _ := path.Match(parts[i], names[j])
		return ok && match(i+1, j+1)
	}
	return match(0, 0)
}

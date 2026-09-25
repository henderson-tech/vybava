// Package gitkit is the deterministic layer the git-family skills (prm,
// push-all, sync) execute: PR selector parsing, review-thread triage, merge
// preconditions, worktree resolution, path classification, DB-url safety.
//
// Skills call the stable surface `vybava gitkit <script> [args]`, never a
// file path. The verbs began as zero-dependency TypeScript run on Node and
// were ported to Go keeping each one's argv grammar, stdout (JSON key order
// included), stderr notes and exit codes byte-identical — native.go holds
// the registry and the Node-compatibility helpers that make that true.
package gitkit

import "sort"

// DiagUnknownScript is the closed diagnostic for a verb gitkit lacks.
const DiagUnknownScript = "GITKIT_UNKNOWN_SCRIPT"

// DiagBadArgs fires when a verb that validates its argv (pr-extensions —
// the ported verbs keep their Node scripts' lenient grammar) gets an
// argument it does not take or a value outside its set; fix is the verb's
// usage line. Exit 2.
const DiagBadArgs = "GITKIT_BAD_ARGS"

// Scripts lists the runnable verbs, sorted.
func Scripts() []string {
	names := make([]string, 0, len(native))
	for name := range native {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

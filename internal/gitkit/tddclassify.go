package gitkit

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// tdd-classify — the deterministic, path-based half of the two-gate TDD
// classifier. A finding's file path maps to a hard-skip category (always
// direct-fix, no test) or "null" (→ apply the behavioral gates).

var skipRules = []struct {
	category string
	patterns []*regexp.Regexp
}{
	// Ordered most-specific → least: ci before generic yaml/iac; migration first.
	{"migration", compile(`(^|/)(migrations?|__migrations__)/`, `(^|/)migration/`)},
	{"deps", compile(`(^|/)(package-lock\.json|pnpm-lock\.yaml|yarn\.lock|bun\.lockb?|composer\.lock|gemfile\.lock|cargo\.lock|go\.sum|poetry\.lock)$`)},
	{"ci", compile(`(^|/)\.github/workflows/`, `(^|/)\.gitlab-ci\.ya?ml$`, `(^|/)\.(circleci|buildkite)/`, `(^|/)azure-pipelines\.ya?ml$`)},
	{"iac", compile(`(^|/)dockerfile(\.[a-z0-9]+)?$`, `docker-compose[^/]*\.ya?ml$`, `\.(tf|tfvars)$`, `(^|/)(nginx|k8s|kubernetes|helm|terraform|ansible)/`, `(^|/)\.env(\.[a-z0-9]+)?$`)},
	{"generated", compile(`\.(generated|gen)\.[a-z]+$`, `\.d\.ts$`, `(^|/)(dist|build|out|\.next|coverage|__generated__|generated)/`)},
	{"docs", compile(`\.(md|mdx|rst|txt)$`, `(^|/)docs?/`)},
}

func compile(patterns ...string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		out[i] = regexp.MustCompile(p)
	}
	return out
}

// SkipCategory returns the hard-skip category for a path, or "" when the
// behavioral gates apply.
func SkipCategory(path string) string {
	p := strings.ToLower(path)
	for _, rule := range skipRules {
		for _, re := range rule.patterns {
			if re.MatchString(p) {
				return rule.category
			}
		}
	}
	return ""
}

func runTDDClassify(args []string, stdout, stderr io.Writer) int {
	path := ""
	for _, a := range args {
		if !strings.HasPrefix(a, "--") {
			path = a
			break
		}
	}
	if path == "" {
		return fail(stderr, errors.New("usage: tdd-classify.ts <path>"))
	}
	category := SkipCategory(path)
	if category == "" {
		category = "null"
	}
	fmt.Fprintln(stdout, category)
	return 0
}

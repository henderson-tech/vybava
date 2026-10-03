package gitkit

import (
	"regexp"
	"strconv"
	"strings"
)

// pr-census's diff split: every changed file lands in one bucket.
//
//   - rest:   lockfiles, generated output, locale catalogs, prose, agent
//     instructions, binary assets — lines nobody reviews as logic;
//   - test:   specs, test directories, fixtures, mocks, snapshots;
//   - config: migrations, CI, infra, deploy, package manifests and tool
//     config — behaviour that is not application code;
//   - code:   everything else.
//
// The first bucket that matches wins, in that order: a README under e2e/ is
// rest, a migration's spec is test. A repo widens a bucket with globs in
// .claude/.claude.git.config (PR_CENSUS_REST_PATHS, PR_CENSUS_TEST_PATHS,
// PR_CENSUS_CONFIG_PATHS); GENERATED_PATHS (else its defaults) counts as
// rest. tdd-classify's categories are reused: deps and generated are rest;
// migration, ci and iac are config. Paths are matched lowercased.

const (
	bucketCode   = "code"
	bucketTest   = "test"
	bucketConfig = "config"
	bucketRest   = "rest"
)

var (
	proseRules  = compile(`\.(md|mdx|rst|txt|adoc)$`)
	agentRules  = compile(`(^|/)\.(claude|agents|codex|cursor)/`)
	localeRules = compile(`(^|/)(i18n|l10n|locales?|translations?|lang)/[^/]+\.(json|ya?ml|po|xlf|arb|properties)$`, `\.(po|pot|xlf|xliff|arb)$`)
	assetRules  = compile(`\.(png|jpe?g|gif|webp|avif|ico|svg|pdf|woff2?|ttf|otf|eot|mp4|webm|mov|mp3|wav|zip|gz)$`)
	testRules   = compile(
		`\.(spec|test|e2e|cy)\.[a-z0-9]+$`, `_test\.go$`, `(^|/)test_[^/]+\.py$`,
		`(^|/)(__tests__|__mocks__|__snapshots__|tests?|specs?|e2e|cypress|playwright|fixtures?|testdata|test-utils)/`,
		`(^|/)[^/]+-e2e/`,
	)
	deployRules   = compile(`(^|/)deploy(ments?|s)?/`, `(^|/)devbox(\.[a-z0-9-]+)?\.ya?ml$`, `(^|/)\.htaccess[^/]*$`)
	manifestRules = compile(`(^|/)(package\.json|go\.mod|composer\.json|cargo\.toml|pyproject\.toml|gemfile)$`)
	toolRules     = compile(
		`(^|/)[^/]+\.config\.[cm]?[jt]s$`, `(^|/)tsconfig[^/]*\.json$`, `(^|/)\.eslintrc[^/]*$`, `(^|/)(nx|project|angular)\.json$`,
		`(^|/)\.(dockerignore|nvmrc|node-version|tool-versions)$`, `(^|/)mise\.toml$`,
	)
)

// splitConfig is a repo's widening of the default rules.
type splitConfig struct {
	rest, test, config []*regexp.Regexp
}

func newSplitConfig(cfg gitConfig) splitConfig {
	globs := func(patterns []string) []*regexp.Regexp {
		var out []*regexp.Regexp
		for _, g := range patterns {
			out = append(out, globToRegExp(strings.ToLower(g)))
		}
		return out
	}
	generated := defaultGeneratedGlobs
	if v, set := cfg.get("GENERATED_PATHS"); set {
		generated = listItems(v)
	}
	return splitConfig{
		rest:   globs(append(append([]string{}, generated...), listItems(cfg["PR_CENSUS_REST_PATHS"])...)),
		test:   globs(listItems(cfg["PR_CENSUS_TEST_PATHS"])),
		config: globs(listItems(cfg["PR_CENSUS_CONFIG_PATHS"])),
	}
}

var listSep = regexp.MustCompile(`[,\s]+`)

// listItems splits a comma- or space-separated config value.
func listItems(v string) []string {
	var out []string
	for _, item := range listSep.Split(strings.TrimSpace(v), -1) {
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

func anyMatch(rules []*regexp.Regexp, p string) bool {
	for _, re := range rules {
		if re.MatchString(p) {
			return true
		}
	}
	return false
}

// bucketOf classifies one changed path.
func (s splitConfig) bucketOf(path string) string {
	p := strings.ToLower(path)
	category := SkipCategory(p)
	switch {
	case category == "deps" || category == "generated" || anyMatch(proseRules, p) || anyMatch(agentRules, p) ||
		anyMatch(localeRules, p) || anyMatch(assetRules, p) || anyMatch(s.rest, p):
		return bucketRest
	case anyMatch(testRules, p) || anyMatch(s.test, p):
		return bucketTest
	case category == "migration" || category == "ci" || category == "iac" || anyMatch(deployRules, p) ||
		anyMatch(manifestRules, p) || anyMatch(toolRules, p) || anyMatch(s.config, p):
		return bucketConfig
	}
	return bucketCode
}

// touchOf names the sensitive area a path touches, "" when none.
func touchOf(path string) string {
	p := strings.ToLower(path)
	switch category := SkipCategory(p); {
	case category == "migration" || category == "ci" || category == "iac" || category == "deps":
		return category
	case anyMatch(manifestRules, p):
		return "deps"
	case anyMatch(deployRules, p):
		return "deploy"
	case anyMatch(localeRules, p):
		return "locale"
	}
	return ""
}

// Lines is one bucket's share of a diff.
type Lines struct {
	Files     int `json:"files"`
	Additions int `json:"additions"`
	Deletions int `json:"deletions"`
}

// Split is a diff in the four buckets, keys in wire order.
type Split struct {
	Code   Lines `json:"code"`
	Test   Lines `json:"test"`
	Config Lines `json:"config"`
	Rest   Lines `json:"rest"`
}

func (s *Split) bucket(name string) *Lines {
	switch name {
	case bucketTest:
		return &s.Test
	case bucketConfig:
		return &s.Config
	case bucketRest:
		return &s.Rest
	}
	return &s.Code
}

// numstatEntry is one file of `git diff --numstat -z`; a binary file counts
// as a file with no lines.
type numstatEntry struct {
	Path      string
	Additions int
	Deletions int
}

// parseNumstatZ reads `git diff --numstat -z -M`. A plain entry is
// "add\tdel\tpath\0"; a rename is "add\tdel\t\0old\0new\0" and counts under
// its new path; a binary file reports "-\t-".
func parseNumstatZ(out string) []numstatEntry {
	fields := strings.Split(out, "\x00")
	var entries []numstatEntry
	for i := 0; i < len(fields); i++ {
		parts := strings.SplitN(fields[i], "\t", 3)
		if len(parts) != 3 {
			continue
		}
		e := numstatEntry{Path: parts[2]}
		e.Additions, _ = strconv.Atoi(parts[0])
		e.Deletions, _ = strconv.Atoi(parts[1])
		if e.Path == "" {
			if i+2 >= len(fields) {
				break
			}
			e.Path = fields[i+2]
			i += 2
		}
		entries = append(entries, e)
	}
	return entries
}

// splitDiff totals a numstat into buckets and lists the sensitive areas it
// touches, in first-seen order.
func (s splitConfig) splitDiff(entries []numstatEntry) (Split, []string) {
	var split Split
	touches := []string{}
	seen := map[string]bool{}
	for _, e := range entries {
		b := split.bucket(s.bucketOf(e.Path))
		b.Files++
		b.Additions += e.Additions
		b.Deletions += e.Deletions
		if t := touchOf(e.Path); t != "" && !seen[t] {
			seen[t] = true
			touches = append(touches, t)
		}
	}
	return split, touches
}

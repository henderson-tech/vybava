package gitkit

import (
	"io"
	"regexp"
	"slices"
	"strings"
)

// classify-paths — partition working-tree paths into commit / secret /
// artifact, so /push-all never commits secret-shaped files or build output.

var (
	secretExt           = []string{".pem", ".key", ".p12", ".pfx", ".keystore", ".jks", ".asc", ".gpg", ".secret", ".token"}
	secretBasenames     = []string{".netrc", ".npmrc", ".pypirc", ".htpasswd", ".dockercfg", ".boto"}
	envTemplateSuffixes = []string{".example", ".sample", ".template", ".dist", ".defaults"}
	artifactSegments    = []string{
		"node_modules", "allure-results", "allure-report", "playwright-report", "test-results",
		"coverage", ".nyc_output", "__pycache__", ".pytest_cache", ".cache", ".next", ".turbo",
	}
	artifactBasenames = []string{".ds_store", "thumbs.db"}
	sshKeyPattern     = regexp.MustCompile(`^id_(rsa|dsa|ecdsa|ed25519)\b`)
)

func hasAnySuffix(s string, suffixes []string) bool {
	return slices.ContainsFunc(suffixes, func(suffix string) bool { return strings.HasSuffix(s, suffix) })
}

// ClassifyPath returns "secret", "artifact" or "commit".
func ClassifyPath(p string) string {
	lower := strings.ToLower(p)
	segs := strings.Split(lower, "/")
	base := segs[len(segs)-1]

	// .env is secret — but .env.example / .sample / .template are committable templates.
	isEnv := base == ".env" || strings.HasPrefix(base, ".env.") || strings.HasSuffix(base, ".env")
	if isEnv && !hasAnySuffix(base, envTemplateSuffixes) {
		return "secret"
	}
	switch {
	case sshKeyPattern.MatchString(base) && !strings.HasSuffix(base, ".pub"),
		hasAnySuffix(base, secretExt),
		slices.Contains(secretBasenames, base),
		base == "credentials" || strings.HasPrefix(base, "credentials."),
		base == "secrets" || strings.HasPrefix(base, "secrets."),
		strings.HasPrefix(base, "service-account") && strings.HasSuffix(base, ".json"),
		strings.HasPrefix(base, "gha-creds-"),
		strings.Contains(base, "credentials") && strings.HasSuffix(base, ".json"):
		return "secret"
	}
	if slices.ContainsFunc(segs, func(s string) bool { return slices.Contains(artifactSegments, s) }) ||
		slices.Contains(artifactBasenames, base) || strings.HasSuffix(base, ".log") {
		return "artifact"
	}
	return "commit"
}

// PathPartition is classify-paths' output, keys in wire order.
type PathPartition struct {
	Commit    []string `json:"commit"`
	Secrets   []string `json:"secrets"`
	Artifacts []string `json:"artifacts"`
}

// ClassifyStatus partitions paths, preserving order within each class.
func ClassifyStatus(paths []string) PathPartition {
	out := PathPartition{Commit: []string{}, Secrets: []string{}, Artifacts: []string{}}
	for _, p := range paths {
		switch ClassifyPath(p) {
		case "secret":
			out.Secrets = append(out.Secrets, p)
		case "artifact":
			out.Artifacts = append(out.Artifacts, p)
		default:
			out.Commit = append(out.Commit, p)
		}
	}
	return out
}

// parsePorcelain extracts paths from `git status --porcelain`: renames take
// the new path, git-quoted paths lose their quotes.
func parsePorcelain(out string) []string {
	paths := []string{}
	for line := range strings.SplitSeq(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		p := ""
		if len(line) > 3 {
			p = line[3:]
		}
		if _, after, found := strings.Cut(p, " -> "); found {
			p = after
		}
		p = strings.TrimPrefix(p, `"`)
		p = strings.TrimSuffix(p, `"`)
		paths = append(paths, p)
	}
	return paths
}

func runClassifyPaths(args []string, stdout, stderr io.Writer) int {
	root, err := repoRoot(args)
	if err != nil {
		return fail(stderr, err)
	}
	out, err := execFile(execOpts{dir: root, echo: stderr, maxBuffer: 64 << 20}, "git", "status", "--porcelain")
	if err != nil {
		return fail(stderr, err)
	}
	if err := writeJSON(stdout, ClassifyStatus(parsePorcelain(out))); err != nil {
		return fail(stderr, err)
	}
	return 0
}

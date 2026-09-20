package memorylint

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/memo"
	"gopkg.in/yaml.v3"
)

type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

type Finding struct {
	Rule     string   `json:"rule"`
	Severity Severity `json:"severity"`
	Path     string   `json:"path"`
	Line     int      `json:"line,omitempty"`
	Message  string   `json:"message"`
}

type Report struct {
	Roots    []string  `json:"roots"`
	Files    int       `json:"files"`
	Findings []Finding `json:"findings"`
}

func (r Report) Errors() int {
	count := 0
	for _, finding := range r.Findings {
		if finding.Severity == SeverityError {
			count++
		}
	}
	return count
}

func (r Report) Warnings() int {
	return len(r.Findings) - r.Errors()
}

type Config struct {
	Version       int      `yaml:"version"`
	MaxIndexLines int      `yaml:"max_index_lines"`
	MaxEntryLines int      `yaml:"max_entry_lines"`
	AllowedTypes  []string `yaml:"allowed_types"`
	AllowedEmails []string `yaml:"allowed_emails"`
	AllowedIPs    []string `yaml:"allowed_ips"`
	AllowedValues []string `yaml:"allowed_values"`
	Ignore        []string `yaml:"ignore"`
	allowedRegex  []*regexp.Regexp
}

type frontmatter struct {
	Name         string `yaml:"name"`
	Description  string `yaml:"description"`
	Type         string `yaml:"type"`
	Status       string `yaml:"status"`
	Expires      string `yaml:"expires"`
	LastUsed     string `yaml:"last-used"`
	LastVerified string `yaml:"last-verified"`
	Metadata     struct {
		Type     string `yaml:"type"`
		Modified string `yaml:"modified"`
	} `yaml:"metadata"`
}

type entry struct {
	path     string
	relative string
	name     string
	noteType string
	data     []byte
}

var (
	errMissingFrontmatter      = errors.New("missing YAML frontmatter")
	errUnterminatedFrontmatter = errors.New("unterminated YAML frontmatter")
)

var (
	markdownLinkPattern = regexp.MustCompile(`\[[^\]]+\]\(([^)]+\.md)(?:#[^)]+)?\)`)
	wikiLinkPattern     = regexp.MustCompile(`\[\[([^\]|#]+)(?:[|#][^\]]*)?\]\]`)
	emailPattern        = regexp.MustCompile(`(?i)\b[A-Z0-9._%+\-]+@[A-Z0-9.\-]+\.[A-Z]{2,}\b`)
	ipv4Pattern         = regexp.MustCompile(`\b(?:[0-9]{1,3}\.){3}[0-9]{1,3}\b`)
	kebabPattern        = regexp.MustCompile(`^(?:user|feedback|project|reference)-[a-z0-9]+(?:-[a-z0-9]+)*\.md$`)
	secretPatterns      = []struct {
		reason  string
		pattern *regexp.Regexp
	}{
		{"GitHub token", regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}`)},
		{"GitHub token", regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}`)},
		{"provider secret key", regexp.MustCompile(`\bsk_(?:live|test)_[A-Za-z0-9]{8,}`)},
		{"API secret key", regexp.MustCompile(`\bsk-[A-Za-z0-9-]{24,}`)},
		{"AWS access key", regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`)},
		{"private key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
		{"bearer token", regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{20,}`)},
		{"credential assignment", regexp.MustCompile(`(?i)\b(?:password|passwd|api[_-]?key|client[_-]?secret)\s*[:=]\s*['"]?[A-Za-z0-9!@#%^&*_+/-]{8,}`)},
	}
)

func DefaultConfig() Config {
	return Config{
		Version: 1, MaxIndexLines: 100, MaxEntryLines: 150,
		AllowedTypes:  []string{"user", "feedback", "project", "reference"},
		AllowedEmails: []string{"*@example.com", "*@example.org", "*@example.net", "*@example.test"},
		AllowedIPs:    []string{"127.*", "192.0.2.*", "198.51.100.*", "203.0.113.*"},
	}
}

func Discover(start string) ([]string, error) {
	if start == "" {
		var err error
		start, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	start, err := filepath.Abs(start)
	if err != nil {
		return nil, err
	}
	candidates := []string{
		filepath.Join(start, ".claude", "memory"),
		filepath.Join(start, ".codex", "memory"),
		filepath.Join(start, ".Codex", "memory"),
		filepath.Join(start, "memory"),
	}
	var roots []string
	for _, candidate := range candidates {
		if info, statErr := os.Stat(candidate); statErr == nil && info.IsDir() {
			roots = append(roots, candidate)
		}
	}
	if len(roots) == 0 {
		if _, statErr := os.Stat(filepath.Join(start, "MEMORY.md")); statErr == nil {
			roots = append(roots, start)
		}
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("no memory home found under %s; pass a memory directory explicitly", start)
	}
	return roots, nil
}

// lintScope maps a requested path onto the home that must be walked and the
// prefix findings are kept under. A whole home lints unscoped; a handoff
// project directory or a single note lints its home and keeps only its own
// findings, so one project never fails on another project's debt.
func lintScope(absolute string, isDir bool) (root, scope string) {
	if home := handoffsHome(absolute); home != "" {
		if home == absolute {
			return home, ""
		}
		return home, absolute
	}
	if isDir {
		return absolute, ""
	}
	return memoryHome(absolute), absolute
}

// memoryHome returns the nearest ancestor of a note that carries a MEMORY.md,
// so a nested note is linted against its whole home (index, siblings, links);
// without one, the note's own directory.
func memoryHome(note string) string {
	dir := filepath.Dir(note)
	for cur := dir; ; cur = filepath.Dir(cur) {
		if _, err := os.Stat(filepath.Join(cur, "MEMORY.md")); err == nil {
			return cur
		}
		if filepath.Dir(cur) == cur {
			return dir
		}
	}
}

// handoffsHome returns the `.claude/handoffs` directory a path sits in or
// under, or "" when the path is not inside one.
func handoffsHome(path string) string {
	if !IsHandoffHome(path) {
		return ""
	}
	for cur := path; ; cur = filepath.Dir(cur) {
		if filepath.Base(cur) == "handoffs" && filepath.Base(filepath.Dir(cur)) == ".claude" {
			return cur
		}
		if filepath.Dir(cur) == cur {
			return ""
		}
	}
}

func underScope(findings []Finding, scope string) []Finding {
	prefix := scope + string(filepath.Separator)
	var kept []Finding
	for _, f := range findings {
		if f.Path == scope || strings.HasPrefix(f.Path, prefix) {
			kept = append(kept, f)
		}
	}
	return kept
}

func countMarkdown(scope string) (int, error) {
	files := 0
	err := filepath.WalkDir(scope, func(path string, item os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !item.IsDir() && strings.EqualFold(filepath.Ext(path), ".md") {
			files++
		}
		return nil
	})
	return files, err
}

func Lint(paths []string) (Report, error) {
	if len(paths) == 0 {
		discovered, err := Discover("")
		if err != nil {
			return Report{}, err
		}
		paths = discovered
	}

	report := Report{}
	for _, path := range paths {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return Report{}, err
		}
		info, err := os.Stat(absolute)
		if err != nil {
			return Report{}, fmt.Errorf("inspect %s: %w", absolute, err)
		}
		root, scope := lintScope(absolute, info.IsDir())
		config, err := loadConfig(root)
		if err != nil {
			return Report{}, err
		}
		lint := lintRoot
		if IsHandoffHome(root) {
			lint = lintHandoffRoot
		}
		findings, files, err := lint(root, config)
		if err != nil {
			return Report{}, err
		}
		if scope != "" {
			findings = underScope(findings, scope)
			if files, err = countMarkdown(scope); err != nil {
				return Report{}, err
			}
		}
		report.Roots = append(report.Roots, absolute)
		report.Files += files
		report.Findings = append(report.Findings, findings...)
	}
	sort.Slice(report.Findings, func(i, j int) bool {
		if report.Findings[i].Path == report.Findings[j].Path {
			if report.Findings[i].Line == report.Findings[j].Line {
				return report.Findings[i].Rule < report.Findings[j].Rule
			}
			return report.Findings[i].Line < report.Findings[j].Line
		}
		return report.Findings[i].Path < report.Findings[j].Path
	})
	return report, nil
}

func lintRoot(root string, config Config) ([]Finding, int, error) {
	var findings []Finding
	ledger := memo.HasLedger(root)
	var entries []entry
	indexes := make(map[string][]byte)
	files := 0

	err := filepath.WalkDir(root, func(path string, item os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if relative != "." && ignored(relative, config.Ignore) {
			if item.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if item.IsDir() || !strings.EqualFold(filepath.Ext(path), ".md") {
			return nil
		}
		if ledger && filepath.Base(path) == memo.LedgerFile {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files++
		if filepath.Base(path) == "MEMORY.md" {
			indexes[path] = data
			if lines := lineCount(data); lines > config.MaxIndexLines {
				findings = append(findings, finding("M003", SeverityError, path, 1, "index has %d lines; maximum is %d", lines, config.MaxIndexLines))
			}
			if len(data) > 25_000 {
				findings = append(findings, finding("M003", SeverityError, path, 1, "index has %d bytes; maximum is 25000", len(data)))
			}
			findings = append(findings, fixtureFindings(path, data, config)...)
			return nil
		}

		parsed, frontmatterLine, err := parseFrontmatter(data)
		if err != nil {
			findings = append(findings, finding("M001", SeverityError, path, frontmatterLine, "%v", err))
		} else {
			if parsed.Metadata.Type != "" {
				findings = append(findings, finding("M001", SeverityError, path, 1, "legacy nested metadata.type; move it to top-level type"))
			}
			if parsed.Name == "" {
				findings = append(findings, finding("M001", SeverityError, path, 1, "frontmatter is missing name"))
			}
			if parsed.Description == "" {
				findings = append(findings, finding("M001", SeverityError, path, 1, "frontmatter is missing description"))
			}
			if !contains(config.AllowedTypes, parsed.Type) {
				findings = append(findings, finding("M001", SeverityError, path, 1, "type %q is not allowed", parsed.Type))
			}
			if parsed.Status != "active" && parsed.Status != "superseded" && parsed.Status != "provisional" {
				findings = append(findings, finding("M001", SeverityError, path, 1, "status must be active, provisional or superseded"))
			}
			findings = append(findings, lifecycleFindings(path, parsed)...)
			findings = append(findings, usageFindings(path, parsed)...)
			stem := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
			if parsed.Name != "" && parsed.Name != stem {
				findings = append(findings, finding("M001", SeverityError, path, 1, "name %q must match filename stem %q", parsed.Name, stem))
			}
		}
		if !ledgerNote(ledger, relative) && !kebabPatternFor(config.AllowedTypes).MatchString(filepath.Base(path)) {
			findings = append(findings, finding("M002", SeverityWarning, path, 1, "filename must be <type>-<kebab-slug>.md"))
		}
		if lines := lineCount(data); lines > config.MaxEntryLines {
			findings = append(findings, finding("M004", SeverityError, path, 1, "memory has %d lines; maximum is %d", lines, config.MaxEntryLines))
		}
		findings = append(findings, fixtureFindings(path, data, config)...)
		entries = append(entries, entry{path: path, relative: relative, name: parsed.Name, noteType: parsed.Type, data: data})
		return nil
	})
	if err != nil {
		return nil, 0, err
	}

	findings = append(findings, linkFindings(root, entries, indexes, ledger)...)
	findings = append(findings, identityFindings(entries)...)
	findings = append(findings, ceilingFindings(root, ceilingEntries(entries, ledger))...)
	if ledger {
		findings = append(findings, ledgerFindings(root)...)
	}
	return findings, files, nil
}

// kebabPatternFor builds the filename rule from the home's configured types, so
// a home that allows an extra type can hold notes named after it. With the four
// defaults hardcoded, `new --type runbook` created a file `check` then warned
// about — the two commands disagreeing about the same note.
func kebabPatternFor(types []string) *regexp.Regexp {
	var quoted []string
	for _, t := range types {
		if t != "" {
			quoted = append(quoted, regexp.QuoteMeta(t))
		}
	}
	if len(quoted) == 0 {
		return kebabPattern
	}
	return regexp.MustCompile(`^(?:` + strings.Join(quoted, "|") + `)-[a-z0-9]+(?:-[a-z0-9]+)*\.md$`)
}

func loadConfig(root string) (Config, error) {
	config := DefaultConfig()
	path := filepath.Join(root, ".memorylint.yaml")
	data, err := os.ReadFile(path)
	configFound := true
	if errors.Is(err, os.ErrNotExist) {
		parentPath := filepath.Join(filepath.Dir(root), ".memorylint.yaml")
		data, err = os.ReadFile(parentPath)
		if errors.Is(err, os.ErrNotExist) {
			configFound = false
			err = nil
		}
		path = parentPath
	}
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}
	if configFound {
		var overrides Config
		if err := yaml.Unmarshal(data, &overrides); err != nil {
			return Config{}, fmt.Errorf("parse %s: %w", path, err)
		}
		if overrides.Version != 0 && overrides.Version != 1 {
			return Config{}, fmt.Errorf("unsupported memorylint config version %d", overrides.Version)
		}
		if overrides.MaxIndexLines > 0 {
			config.MaxIndexLines = overrides.MaxIndexLines
		}
		if overrides.MaxEntryLines > 0 {
			config.MaxEntryLines = overrides.MaxEntryLines
		}
		if overrides.AllowedTypes != nil {
			config.AllowedTypes = overrides.AllowedTypes
		}
		config.AllowedEmails = append(config.AllowedEmails, overrides.AllowedEmails...)
		config.AllowedIPs = append(config.AllowedIPs, overrides.AllowedIPs...)
		config.AllowedValues = append(config.AllowedValues, overrides.AllowedValues...)
		config.Ignore = overrides.Ignore
	}
	legacyPath := filepath.Join(root, ".memory-lint-allow")
	legacy, legacyErr := os.Open(legacyPath)
	if legacyErr == nil {
		defer func() { _ = legacy.Close() }()
		scanner := bufio.NewScanner(legacy)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			compiled, compileErr := regexp.Compile(line)
			if compileErr != nil {
				return Config{}, fmt.Errorf("parse %s: invalid regex %q: %w", legacyPath, line, compileErr)
			}
			config.allowedRegex = append(config.allowedRegex, compiled)
		}
		if scanErr := scanner.Err(); scanErr != nil {
			return Config{}, fmt.Errorf("read %s: %w", legacyPath, scanErr)
		}
	} else if !errors.Is(legacyErr, os.ErrNotExist) {
		return Config{}, fmt.Errorf("read %s: %w", legacyPath, legacyErr)
	}
	return config, nil
}

func parseFrontmatter(data []byte) (frontmatter, int, error) {
	var result frontmatter
	normalized := bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	if !bytes.HasPrefix(normalized, []byte("---\n")) {
		return result, 1, errMissingFrontmatter
	}
	end := bytes.Index(normalized[4:], []byte("\n---"))
	if end < 0 {
		return result, 1, errUnterminatedFrontmatter
	}
	if err := yaml.Unmarshal(normalized[4:4+end], &result); err != nil {
		return result, 1, fmt.Errorf("invalid YAML frontmatter: %w", err)
	}
	return result, 1, nil
}

func linkFindings(root string, entries []entry, indexes map[string][]byte, ledger bool) []Finding {
	var findings []Finding
	knownWiki := make(map[string]struct{})
	if ledger {
		knownWiki["ledger"] = struct{}{}
	}
	knownPaths := make(map[string]entry)
	indexedPaths := make(map[string]struct{})
	for _, value := range entries {
		knownPaths[filepath.Clean(value.path)] = value
		knownWiki[strings.ToLower(strings.TrimSuffix(filepath.Base(value.path), filepath.Ext(value.path)))] = struct{}{}
		if value.name != "" {
			knownWiki[strings.ToLower(value.name)] = struct{}{}
		}
	}

	for indexPath, data := range indexes {
		for _, match := range markdownLinkPattern.FindAllSubmatchIndex(data, -1) {
			targetText := string(data[match[2]:match[3]])
			targetText = strings.SplitN(targetText, "#", 2)[0]
			if strings.Contains(targetText, "://") {
				continue
			}
			target := filepath.Clean(filepath.Join(filepath.Dir(indexPath), filepath.FromSlash(targetText)))
			line := lineAt(data, match[0])
			if _, err := os.Stat(target); err != nil {
				findings = append(findings, finding("M005", SeverityError, indexPath, line, "index target does not exist: %s", targetText))
			} else if _, isMemory := knownPaths[target]; isMemory {
				indexedPaths[target] = struct{}{}
			}
		}
	}
	for _, value := range entries {
		if _, indexed := indexedPaths[filepath.Clean(value.path)]; !indexed && !ledger {
			findings = append(findings, finding("M009", SeverityWarning, value.path, 1, "memory is not linked from a MEMORY.md index under %s", root))
		}
		for _, match := range wikiLinkPattern.FindAllSubmatchIndex(value.data, -1) {
			target := strings.ToLower(strings.TrimSpace(string(value.data[match[2]:match[3]])))
			target = strings.TrimSuffix(target, ".md")
			if ledger && crossHomeLink(target) {
				continue
			}
			if strings.Contains(target, "/") {
				target = strings.TrimSuffix(filepath.Base(target), ".md")
			}
			if _, exists := knownWiki[target]; !exists {
				findings = append(findings, finding("M006", SeverityError, value.path, lineAt(value.data, match[0]), "wikilink target does not exist: %s", target))
			}
		}
	}
	return findings
}

// lifecycleFindings enforces the provisional lifecycle: a provisional note must
// carry a well-formed expires date, promotion to any other status drops it, and
// an expired provisional is deletable on sight — no re-litigation.
func lifecycleFindings(path string, parsed frontmatter) []Finding {
	if parsed.Status != "provisional" {
		if parsed.Expires != "" {
			return []Finding{finding("M012", SeverityError, path, 1, "expires is only valid on a provisional note; promotion drops it")}
		}
		return nil
	}
	if parsed.Expires == "" {
		return []Finding{finding("M012", SeverityError, path, 1, "provisional note must carry expires: YYYY-MM-DD")}
	}
	if _, err := time.Parse("2006-01-02", parsed.Expires); err != nil {
		return []Finding{finding("M012", SeverityError, path, 1, "expires %q is not a YYYY-MM-DD date", parsed.Expires)}
	}
	// Zero-padded ISO dates compare correctly as strings, and Format uses local
	// time — so "today" flips exactly at the user's midnight, not UTC's.
	if parsed.Expires < time.Now().Format("2006-01-02") {
		return []Finding{finding("M013", SeverityWarning, path, 1, "expired provisional — deletable on sight (expired %s)", parsed.Expires)}
	}
	return nil
}

// Doctrine constants, deliberately not configurable: hitting a ceiling forces
// consolidation or eviction, never a cap raise.
const (
	evictionWindowDays  = 90
	personalNoteCeiling = 15
	teamNoteCeiling     = 30
)

// usageFindings enforces the forgetting half of the lifecycle: a non-provisional
// note whose most recent usage signal (last-used, last-verified, or the legacy
// metadata.modified) is older than the eviction window is an eviction candidate.
// A note carrying none of the three is skipped — no date is not evidence of
// disuse, and guessing one would mark every legacy note for eviction at once.
func usageFindings(path string, parsed frontmatter) []Finding {
	var findings []Finding
	if parsed.LastUsed != "" {
		if _, err := time.Parse("2006-01-02", parsed.LastUsed); err != nil {
			findings = append(findings, finding("M012", SeverityError, path, 1, "last-used %q is not a YYYY-MM-DD date", parsed.LastUsed))
			return findings
		}
	}
	if parsed.Status == "provisional" {
		return findings // expires already bounds its life
	}
	newest := ""
	for _, candidate := range []string{parsed.LastUsed, parsed.LastVerified, parsed.Metadata.Modified} {
		if len(candidate) >= 10 {
			candidate = candidate[:10] // metadata.modified is an RFC3339 stamp
		}
		if _, err := time.Parse("2006-01-02", candidate); err != nil {
			continue
		}
		if candidate > newest {
			newest = candidate
		}
	}
	if newest == "" {
		return findings
	}
	if newest < time.Now().AddDate(0, 0, -evictionWindowDays).Format("2006-01-02") {
		findings = append(findings, finding("M014", SeverityWarning, path, 1, "unused for %dd — eviction candidate (last signal %s)", evictionWindowDays, newest))
	}
	return findings
}

// ceilingFindings enforces the note-count cap per home: every surviving note
// competes with the actual rules for instruction-following budget, so a home
// over its ceiling gets one home-level warning. A home holding only personal
// types (user/feedback) is a personal home with the tighter ceiling.
func ceilingFindings(root string, entries []entry) []Finding {
	if len(entries) == 0 {
		return nil
	}
	ceiling := personalNoteCeiling
	for _, value := range entries {
		if value.noteType != "user" && value.noteType != "feedback" {
			ceiling = teamNoteCeiling
			break
		}
	}
	if len(entries) <= ceiling {
		return nil
	}
	return []Finding{finding("M015", SeverityWarning, root, 0, "home holds %d notes; ceiling is %d — consolidate or evict, never raise the cap", len(entries), ceiling)}
}

func identityFindings(entries []entry) []Finding {
	var findings []Finding
	byName := make(map[string]string)
	for _, value := range entries {
		if value.name == "" {
			continue
		}
		key := strings.ToLower(value.name)
		if previous, exists := byName[key]; exists {
			findings = append(findings, finding("M010", SeverityError, value.path, 1, "duplicate memory name %q; also used by %s", value.name, previous))
		} else {
			byName[key] = value.path
		}
	}
	return findings
}

func fixtureFindings(path string, data []byte, config Config) []Finding {
	findings := secretFindings(path, data, config)
	for _, location := range emailPattern.FindAllIndex(data, -1) {
		value := string(data[location[0]:location[1]])
		if !allowed(value, config.AllowedEmails, config.allowedRegex) {
			findings = append(findings, finding("M007", SeverityError, path, lineAt(data, location[0]), "email is not allowlisted: %s", value))
		}
	}
	for _, location := range ipv4Pattern.FindAllIndex(data, -1) {
		value := string(data[location[0]:location[1]])
		if net.ParseIP(value) == nil || allowed(value, config.AllowedIPs, config.allowedRegex) {
			continue
		}
		findings = append(findings, finding("M008", SeverityError, path, lineAt(data, location[0]), "IP address is not allowlisted: %s", value))
	}
	return findings
}

// secretFindings is the token scan alone: handoffs are machine-local and name
// servers and people freely, so only real credential material blocks there.
func secretFindings(path string, data []byte, config Config) []Finding {
	var findings []Finding
	for _, secret := range secretPatterns {
		for _, location := range secret.pattern.FindAllIndex(data, -1) {
			value := string(data[location[0]:location[1]])
			if allowed(value, config.AllowedValues, config.allowedRegex) {
				continue
			}
			redacted := value
			if len(redacted) > 20 {
				redacted = redacted[:20] + "…"
			}
			findings = append(findings, finding("M011", SeverityError, path, lineAt(data, location[0]), "%s is not allowed in memory: %s", secret.reason, redacted))
		}
	}
	return findings
}

func ignored(relative string, patterns []string) bool {
	for _, pattern := range patterns {
		matched, err := filepath.Match(filepath.FromSlash(pattern), filepath.FromSlash(relative))
		if err == nil && matched {
			return true
		}
		if strings.HasSuffix(pattern, "/**") && strings.HasPrefix(relative, strings.TrimSuffix(pattern, "**")) {
			return true
		}
	}
	return false
}

func matchesAny(value string, patterns []string) bool {
	for _, pattern := range patterns {
		matched, err := filepath.Match(strings.ToLower(pattern), strings.ToLower(value))
		if err == nil && matched {
			return true
		}
	}
	return false
}

func allowed(value string, patterns []string, expressions []*regexp.Regexp) bool {
	if matchesAny(value, patterns) {
		return true
	}
	for _, expression := range expressions {
		if expression.MatchString(value) {
			return true
		}
	}
	return false
}

func lineCount(data []byte) int {
	if len(data) == 0 {
		return 0
	}
	count := bytes.Count(data, []byte("\n"))
	if data[len(data)-1] != '\n' {
		count++
	}
	return count
}

func lineAt(data []byte, offset int) int {
	return bytes.Count(data[:offset], []byte("\n")) + 1
}

func finding(rule string, severity Severity, path string, line int, format string, args ...any) Finding {
	return Finding{Rule: rule, Severity: severity, Path: path, Line: line, Message: fmt.Sprintf(format, args...)}
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func FormatText(report Report) string {
	var output strings.Builder
	for _, value := range report.Findings {
		_, _ = fmt.Fprintf(&output, "%s:%d: %s %s %s\n", value.Path, value.Line, value.Severity, value.Rule, value.Message)
	}
	if len(report.Findings) == 0 {
		_, _ = fmt.Fprintf(&output, "memorylint: clean (%d files in %d homes)\n", report.Files, len(report.Roots))
	} else {
		_, _ = fmt.Fprintf(&output, "memorylint: %d errors, %d warnings (%d files)\n", report.Errors(), report.Warnings(), report.Files)
	}
	return output.String()
}

// Ledger homes (docs/memo.md): LEDGER.md is memo's, notes live under notes/
// with plain kebab-case names, cross-home wikilinks are resolved by memo
// against registered aliases, and the note ceiling counts notes/ only.

var notesNamePattern = regexp.MustCompile(`^notes/[a-z0-9]+(?:-[a-z0-9]+)*\.md$`)

func ledgerNote(ledger bool, relative string) bool {
	return ledger && notesNamePattern.MatchString(relative)
}

// crossHomeLink is `<alias>/LEDGER` or `<alias>/notes/<slug>`; a local
// `notes/<slug>` or `LEDGER` is not.
func crossHomeLink(target string) bool {
	return strings.Contains(target, "/") && !strings.HasPrefix(target, "notes/")
}

func ceilingEntries(entries []entry, ledger bool) []entry {
	if !ledger {
		return entries
	}
	var kept []entry
	for _, e := range entries {
		if strings.HasPrefix(e.relative, "notes/") {
			kept = append(kept, e)
		}
	}
	return kept
}

// ledgerFindings runs memo's ledger rules and maps them onto this report.
func ledgerFindings(root string) []Finding {
	var homes []memo.Home
	if userHome, err := os.UserHomeDir(); err == nil {
		homes, _, _ = memo.Env{UserHome: userHome, Cwd: root}.Discover()
	}
	var out []Finding
	for _, f := range memo.Lint(root, memo.AliasMap(homes), time.Now()) {
		out = append(out, Finding{Rule: f.Rule, Severity: Severity(f.Severity), Path: f.Path, Line: f.Line, Message: f.Message})
	}
	return out
}

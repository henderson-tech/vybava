package gitkit

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
)

// sync-context — resolve /sync's per-project plan from
// <repo>/.claude/.claude.git.config (KEY=value) merged with auto-detection.
// `sync-context --repo <abs> [--freeze]` prints the plan JSON. --freeze
// writes every resolved-but-unconfigured key back into the config file so
// the NEXT run is a read + execute with no discovery.

// gitConfig is a parsed .claude.git.config: presence matters (an empty
// value still counts as set, as `??` does in the TypeScript).
type gitConfig map[string]string

// get returns the value and whether the key is set at all.
func (c gitConfig) get(key string) (string, bool) {
	v, ok := c[key]
	return v, ok
}

// or returns the configured value, else fallback (nil = JSON null).
func (c gitConfig) or(key string, fallback *string) *string {
	if v, ok := c[key]; ok {
		return &v
	}
	return fallback
}

// readGitConfig reads <root>/.claude/.claude.git.config (committed, shared)
// overlaid by .claude.git.config.local (gitignored — one machine's own
// defaults). A key in .local wins; either file may be absent — but one that
// exists and cannot be read is an error, never a silently empty config.
func readGitConfig(root string) (gitConfig, bool, error) {
	merged := gitConfig{}
	found := false
	for _, name := range []string{".claude/.claude.git.config", ".claude/.claude.git.config.local"} {
		data, present, err := readIfPresent(filepath.Join(root, name))
		if err != nil {
			return nil, false, err
		}
		if !present {
			continue
		}
		found = true
		for k, v := range parseConfig(string(data)) {
			merged[k] = v
		}
	}
	return merged, found, nil
}

// ReadGitConfig is readGitConfig for other packages (claude-guards reads
// PROD_BRANCHES from the same files, parsed the same way): the merged
// KEY=value map, empty when neither file exists.
func ReadGitConfig(root string) (map[string]string, error) {
	cfg, _, err := readGitConfig(root)
	return cfg, err
}

// readIfPresent reads a file that may legitimately be absent. Only a path
// that is not there is absent; anything else — a directory, a permission
// error on the way — is an error rendered as Node's, never absence, which
// would silently apply defaults (safety defaults, for the git config).
func readIfPresent(path string) ([]byte, bool, error) {
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR):
		return nil, false, nil
	case err != nil:
		return nil, true, nodeReadError(err, path)
	}
	return data, true, nil
}

// nodeReadError renders a readFileSync failure as Node does.
func nodeReadError(err error, path string) error {
	if errors.Is(err, syscall.EISDIR) {
		return errors.New("EISDIR: illegal operation on a directory, read")
	}
	return nodeFSError(err, "open", path)
}

func parseConfig(text string) gitConfig {
	out := gitConfig{}
	for raw := range strings.SplitSeq(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		if len(val) >= 2 && (val[0] == '"' && val[len(val)-1] == '"' || val[0] == '\'' && val[len(val)-1] == '\'') {
			val = val[1 : len(val)-1]
		} else if val == `"` || val == `'` {
			val = "" // JS slice(1, -1) of a lone quote
		}
		if key != "" {
			out[key] = val
		}
	}
	return out
}

func packageManager(files []string) string {
	switch {
	case slices.Contains(files, "pnpm-lock.yaml"):
		return "pnpm"
	case slices.Contains(files, "bun.lock"), slices.Contains(files, "bun.lockb"):
		return "bun"
	case slices.Contains(files, "yarn.lock"):
		return "yarn"
	case slices.Contains(files, "package-lock.json"):
		return "npm"
	}
	return ""
}

func installCmdForLockfile(files []string) *string {
	if pm := packageManager(files); pm != "" {
		cmd := pm + " install"
		return &cmd
	}
	return nil
}

var localDBHosts = []string{"localhost", "127.0.0.1", "0.0.0.0", "::1", "host.docker.internal", "db"}

// dbHostOf is what the URL parser says the host is — a substring search
// would read `postgres://u@localhost:5432@prod.example.com/app` as local.
// Never returns credentials.
func dbHostOf(raw string) *string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return nil
	}
	host := u.Hostname()
	if host == "" {
		return nil
	}
	return &host
}

func isLocalDBURL(raw string) bool {
	host := dbHostOf(raw)
	return host != nil && slices.Contains(localDBHosts, strings.ToLower(*host))
}

func scriptCmd(pm, name string) string {
	switch pm {
	case "npm":
		return "npm run " + name
	case "yarn":
		return "yarn " + name
	}
	return pm + " run " + name
}

// truthy is JavaScript truthiness for a decoded JSON value.
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case float64:
		return t != 0
	case string:
		return t != ""
	}
	return true
}

func pickScript(scripts map[string]any, candidates ...string) string {
	for _, c := range candidates {
		if truthy(scripts[c]) {
			return c
		}
	}
	return ""
}

// defaultGeneratedGlobs is the "generated, never hand-merge" fallback when a
// repo sets no GENERATED_PATHS. Deliberately conservative: a false positive
// means regenerating over someone's hand-written code.
var defaultGeneratedGlobs = []string{
	"**/generated/**",
	"**/__generated__/**",
	"**/*.gen.*",
	"**/*.generated.*",
	"**/openapi*.json",
	"**/openapi*.yaml",
	"**/openapi*.yml",
	"**/schema.graphql",
	"**/graphql.schema.json",
	"**/prisma/client/**",
}

// globToRegExp translates one glob into an anchored regexp: `*` stops at
// `/`, `**` crosses it, `**/` may match zero segments, {a,b} alternates.
func globToRegExp(pattern string) *regexp.Regexp {
	var out strings.Builder
	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; c {
		case '*':
			switch {
			case i+1 < len(pattern) && pattern[i+1] == '*' && i+2 < len(pattern) && pattern[i+2] == '/':
				out.WriteString("(?:.*/)?")
				i += 2
			case i+1 < len(pattern) && pattern[i+1] == '*':
				out.WriteString(".*")
				i++
			default:
				out.WriteString("[^/]*")
			}
		case '?':
			out.WriteString("[^/]")
		case '{':
			end := strings.IndexByte(pattern[i:], '}')
			if end == -1 {
				out.WriteString(`\{`)
				continue
			}
			alts := strings.Split(pattern[i+1:i+end], ",")
			for j, alt := range alts {
				alts[j] = regexp.QuoteMeta(alt)
			}
			out.WriteString("(?:" + strings.Join(alts, "|") + ")")
			i += end
		default:
			if strings.IndexByte(`.+^${}()|[]\`, c) >= 0 {
				out.WriteByte('\\')
			}
			out.WriteByte(c)
		}
	}
	return regexp.MustCompile("^" + out.String() + "$")
}

// isGeneratedPath: a pattern with no glob metacharacter is a path prefix
// (segment-bounded); a pattern with no `/` also matches the basename.
func isGeneratedPath(path string, patterns []string) bool {
	p := strings.TrimPrefix(path, "./")
	base := p[strings.LastIndex(p, "/")+1:]
	for _, raw := range patterns {
		pattern := strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(raw), "./"), "/")
		if pattern == "" {
			continue
		}
		if !strings.ContainsAny(pattern, "*?{") {
			if p == pattern || strings.HasPrefix(p, pattern+"/") {
				return true
			}
			continue
		}
		re := globToRegExp(pattern)
		if re.MatchString(p) || (!strings.Contains(pattern, "/") && re.MatchString(base)) {
			return true
		}
	}
	return false
}

// detectMode: branch identity decides which half of /sync runs — never the
// worktree location.
func detectMode(currentBranch, defaultBranch string) string {
	if currentBranch == defaultBranch {
		return "main"
	}
	return "branch"
}

// kv is one ordered config entry; nil values are never frozen.
type kv struct {
	key   string
	value *string
}

// freezeConfig appends the resolved keys the file does not set yet. It never
// rewrites a key the file already sets and is idempotent.
func freezeConfig(existing string, resolved []kv, today string) (string, []string) {
	have := parseConfig(existing)
	added := []string{}
	var block strings.Builder
	for _, e := range resolved {
		if e.value == nil || *e.value == "" {
			continue
		}
		if _, set := have[e.key]; set {
			continue
		}
		fmt.Fprintf(&block, "%s=%s\n", e.key, *e.value)
		added = append(added, e.key)
	}
	if len(added) == 0 {
		return existing, added
	}
	head := existing
	if existing != "" && !strings.HasSuffix(existing, "\n") {
		head += "\n"
	}
	return head + "\n# auto-detected by /sync " + today + " — edit freely, /sync never overwrites\n" + block.String(), added
}

// SyncPlan is sync-context's output, keys in wire order.
type SyncPlan struct {
	ConfigFound         bool     `json:"configFound"`
	ConfigPath          string   `json:"configPath"`
	Mode                string   `json:"mode"`
	CurrentBranch       string   `json:"currentBranch"`
	Upstream            *string  `json:"upstream"`
	IsWorktree          bool     `json:"isWorktree"`
	DefaultBranch       string   `json:"defaultBranch"`
	GeneratedPaths      []string `json:"generatedPaths"`
	GeneratedFromConfig bool     `json:"generatedFromConfig"`
	RegenCmd            *string  `json:"regenCmd"`
	VerifyCmd           *string  `json:"verifyCmd"`
	MissingKeys         []string `json:"missingKeys"`
	ConfigComplete      bool     `json:"configComplete"`
	MergeStrategy       string   `json:"mergeStrategy"`
	PackageManager      *string  `json:"packageManager"`
	InstallCmd          *string  `json:"installCmd"`
	Lockfiles           []string `json:"lockfiles"`
	MigrateCmd          *string  `json:"migrateCmd"`
	BackupCmd           *string  `json:"backupCmd"`
	MigrationsPaths     []string `json:"migrationsPaths"`
	RestartCmd          *string  `json:"restartCmd"`
	HasComposeFile      bool     `json:"hasComposeFile"`
	LocalDBURLVar       string   `json:"localDbUrlVar"`
	DBHost              *string  `json:"dbHost"`
	LocalDBOK           *bool    `json:"localDbOk"`
	RunAfterSync        *string  `json:"runAfterSync"`
}

func splitList(s string) []string {
	out := []string{}
	for part := range strings.SplitSeq(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// syncContextArgs: --freeze writes the resolved keys back; no positionals.
var syncContextArgs = verbArgs{values: []string{"repo"}, bools: []string{"freeze"}, usage: "usage: vybava gitkit sync-context [--repo <abs>] [--freeze]"}

func runSyncContext(args []string, stdout, stderr io.Writer) int {
	flags, _, err := syncContextArgs.parse("sync-context", args)
	if err != nil {
		return fail(stderr, err)
	}
	// Anchored: a drifted shell cwd must not resolve (and then migrate or
	// install against) a DIFFERENT repo.
	root, err := repoRoot(repoAnchor(flags))
	if err != nil {
		return fail(stderr, err)
	}
	at := func(p string) string { return filepath.Join(root, p) }
	configPath := at(".claude/.claude.git.config") // --freeze writes the committed file
	cfg, configFound, err := readGitConfig(root)
	if err != nil {
		return fail(stderr, err)
	}

	lockfiles := []string{}
	for _, f := range []string{"pnpm-lock.yaml", "bun.lock", "bun.lockb", "yarn.lock", "package-lock.json"} {
		if exists(at(f)) {
			lockfiles = append(lockfiles, f)
		}
	}
	pm := packageManager(lockfiles)

	scripts := map[string]any{}
	if data, present, err := readIfPresent(at("package.json")); err != nil {
		fmt.Fprintf(stderr, "warn: could not parse package.json (%s)\n", err)
	} else if present {
		// Malformed package.json — recover to no scripts rather than crash the
		// plan. A non-object top level or scripts field reads as no scripts,
		// as property access does in JS; only null throws there.
		var pkg any
		if err := json.Unmarshal(data, &pkg); err != nil {
			fmt.Fprintf(stderr, "warn: could not parse package.json (%s)\n", err)
		} else if pkg == nil {
			fmt.Fprintln(stderr, "warn: could not parse package.json (Cannot read properties of null (reading 'scripts'))")
		} else if obj, ok := pkg.(map[string]any); ok {
			if s, ok := obj["scripts"].(map[string]any); ok {
				scripts = s
			}
		}
	}

	defaultBranch, _ := cfg.get("DEFAULT_BRANCH")
	quiet := execOpts{dir: root}
	if defaultBranch == "" {
		out, err := execFile(quiet, "git", "symbolic-ref", "--short", "refs/remotes/origin/HEAD")
		if err != nil {
			defaultBranch = "main" // origin/HEAD not set
		} else {
			defaultBranch = strings.TrimPrefix(strings.TrimSpace(out), "origin/")
		}
	}

	script := func(candidates ...string) *string {
		if pm == "" {
			return nil
		}
		if name := pickScript(scripts, candidates...); name != "" {
			cmd := scriptCmd(pm, name)
			return &cmd
		}
		return nil
	}
	migrate := script("db:migrate", "migrate", "migration:run", "prisma:migrate")
	backup := script("db:backup", "backup")
	regen := script("api:generate", "codegen", "gen:types", "generate", "orval", "openapi:generate", "prisma:generate")
	verify := script("typecheck", "type-check", "tsc", "check", "build")

	// Branch identity picks the mode; worktree-ness is reported but never
	// decides. A detached HEAD reports "HEAD", matches no branch, and lands in
	// branch mode where the skill stops on its own guard.
	gitOut := func(a ...string) *string {
		out, err := execFile(quiet, "git", a...)
		if err != nil {
			return nil
		}
		s := strings.TrimSpace(out)
		return &s
	}
	currentBranch := "HEAD"
	if b := gitOut("rev-parse", "--abbrev-ref", "HEAD"); b != nil {
		currentBranch = *b
	}
	upstream := gitOut("rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	gitDir := gitOut("rev-parse", "--absolute-git-dir")
	gitCommonDir := gitOut("rev-parse", "--path-format=absolute", "--git-common-dir")
	isWorktree := gitDir != nil && *gitDir != "" && gitCommonDir != nil && *gitCommonDir != "" && *gitDir != *gitCommonDir

	// Deterministic local-DB guard: read the DB-URL var from env / .env and
	// emit only localDbOk + host (NEVER the URL). null = unknown → STOP.
	localDBURLVar := "DATABASE_URL"
	if v, ok := cfg.get("LOCAL_DB_URL_VAR"); ok {
		localDBURLVar = v
	}
	dbURL := os.Getenv(localDBURLVar)
	if dbURL == "" {
		line := regexp.MustCompile("(?m)^" + regexp.QuoteMeta(localDBURLVar) + "=(.*)$")
		for _, f := range []string{".env.local", ".env"} {
			// Present but unreadable fails the plan: reading it as absent
			// would report the DB state unknown for the wrong reason.
			data, present, err := readIfPresent(at(f))
			if err != nil {
				return fail(stderr, err)
			}
			if !present {
				continue
			}
			if m := line.FindStringSubmatch(string(data)); m != nil {
				dbURL = strings.TrimSpace(m[1])
				if dbURL != "" && strings.ContainsAny(dbURL[:1], `"'`) {
					dbURL = dbURL[1:]
				}
				if dbURL != "" && strings.ContainsAny(dbURL[len(dbURL)-1:], `"'`) {
					dbURL = dbURL[:len(dbURL)-1]
				}
				break
			}
		}
	}
	var dbHost *string
	var localDBOK *bool
	if dbURL != "" {
		dbHost = dbHostOf(dbURL)
		ok := isLocalDBURL(dbURL)
		localDBOK = &ok
	}

	generatedPaths := defaultGeneratedGlobs
	configuredGenerated, _ := cfg.get("GENERATED_PATHS")
	if configuredGenerated != "" {
		generatedPaths = splitList(configuredGenerated)
	}
	regenCmd := cfg.or("REGEN_CMD", regen)
	verifyCmd := cfg.or("VERIFY_CMD", verify)
	installCmd := cfg.or("INSTALL_CMD", installCmdForLockfile(lockfiles))
	migrateCmd := cfg.or("DB_MIGRATE_CMD", migrate)
	backupCmd := cfg.or("DB_BACKUP_CMD", backup)
	restartCmd := cfg.or("RESTART_CMD", nil)
	joinedGenerated := strings.Join(generatedPaths, ",")

	// The keys worth freezing: each is an exploration the model would redo,
	// or a detection that could drift. Nil values are never written.
	freezable := []kv{
		{"DEFAULT_BRANCH", &defaultBranch},
		{"INSTALL_CMD", installCmd},
		{"GENERATED_PATHS", &joinedGenerated},
		{"REGEN_CMD", regenCmd},
		{"VERIFY_CMD", verifyCmd},
		{"DB_MIGRATE_CMD", migrateCmd},
		{"DB_BACKUP_CMD", backupCmd},
		{"RESTART_CMD", restartCmd},
	}
	// "Complete" = the exploration-heavy keys are written down (fast path).
	missingKeys := []string{}
	for _, e := range freezable[:5] {
		if _, set := cfg[e.key]; !set && e.value != nil {
			missingKeys = append(missingKeys, e.key)
		}
	}

	if _, freeze := flags["freeze"]; freeze {
		before := ""
		if configFound {
			data, err := os.ReadFile(configPath)
			if err != nil {
				return fail(stderr, fmt.Errorf("ENOENT: no such file or directory, open '%s'", configPath))
			}
			before = string(data)
		}
		text, added := freezeConfig(before, freezable, time.Now().UTC().Format("2006-01-02"))
		note := "nothing to add"
		if len(added) > 0 {
			if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
				return fail(stderr, err)
			}
			if err := os.WriteFile(configPath, []byte(text), 0o644); err != nil {
				return fail(stderr, err)
			}
			note = "wrote " + strings.Join(added, ", ") + " to " + configPath
		}
		fmt.Fprintf(stderr, "freeze: %s\n", note)
	}

	var pmOut *string
	if pm != "" {
		pmOut = &pm
	}
	mergeStrategy := "merge"
	if v, ok := cfg.get("MERGE_STRATEGY"); ok {
		mergeStrategy = strings.ToLower(v)
	}
	migrationsPaths := "migrations,prisma/migrations,apps/api/migrations,db/migrate"
	if v, ok := cfg.get("MIGRATIONS_PATHS"); ok {
		migrationsPaths = v
	}
	plan := SyncPlan{
		ConfigFound:         configFound,
		ConfigPath:          configPath,
		Mode:                detectMode(currentBranch, defaultBranch),
		CurrentBranch:       currentBranch,
		Upstream:            upstream,
		IsWorktree:          isWorktree,
		DefaultBranch:       defaultBranch,
		GeneratedPaths:      generatedPaths,
		GeneratedFromConfig: configuredGenerated != "",
		RegenCmd:            regenCmd,
		VerifyCmd:           verifyCmd,
		MissingKeys:         missingKeys,
		ConfigComplete:      len(missingKeys) == 0,
		MergeStrategy:       mergeStrategy,
		PackageManager:      pmOut,
		InstallCmd:          installCmd,
		Lockfiles:           lockfiles,
		MigrateCmd:          migrateCmd,
		BackupCmd:           backupCmd,
		MigrationsPaths:     splitList(migrationsPaths),
		RestartCmd:          restartCmd,
		HasComposeFile:      exists(at("docker-compose.yml")) || exists(at("docker-compose.yaml")) || exists(at("compose.yml")),
		LocalDBURLVar:       localDBURLVar,
		DBHost:              dbHost,
		LocalDBOK:           localDBOK,
		RunAfterSync:        cfg.or("RUN_AFTER_SYNC", nil),
	}
	if err := writeJSON(stdout, plan); err != nil {
		return fail(stderr, err)
	}
	return 0
}

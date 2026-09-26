package memo

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/henderson-tech/vybava/internal/gitkit"
	"github.com/henderson-tech/vybava/internal/shellword"
)

// Home is one ledger directory with its alias and kind.
type Home struct {
	Alias  string `json:"alias"`
	Path   string `json:"path"`
	Kind   Kind   `json:"kind"`
	Source string `json:"source"` // discovered | registered | session
}

// Registry is ~/.config/vybava/memo/homes.json: alias overrides and homes
// discovery cannot see. Unknown fields are rejected.
type Registry struct {
	Version int             `json:"version"`
	Homes   []RegistryEntry `json:"homes"`
}

// RegistryEntry maps an alias onto a home path.
type RegistryEntry struct {
	Alias string `json:"alias"`
	Path  string `json:"path"`
}

// Env is the process context every home resolution needs; tests build it
// from temp dirs, the CLI from the real process.
type Env struct {
	UserHome string // $HOME
	Cwd      string
	Session  string // CLAUDE_CODE_SESSION_ID, may be empty
}

var (
	personalHomeRE = regexp.MustCompile(`/\.(?:claude|codex)/projects/[^/]+/memory$`)
	aliasRE        = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
)

// RegistryPath is where homes.json lives for this user.
func (e Env) RegistryPath() string {
	return filepath.Join(e.UserHome, ".config", "vybava", "memo", "homes.json")
}

// LoadRegistry reads homes.json strictly; a missing file is an empty registry.
func (e Env) LoadRegistry() (Registry, *Diag, error) {
	raw, err := os.ReadFile(e.RegistryPath())
	if os.IsNotExist(err) {
		return Registry{Version: 1}, nil, nil
	}
	if err != nil {
		return Registry{}, nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var reg Registry
	if err := dec.Decode(&reg); err != nil {
		return Registry{}, errorDiag(DiagRegistryInvalid, fmt.Sprintf("%s: %v", e.RegistryPath(), err), "memo homes register <alias> <path>  # rewrites the file"), nil
	}
	for _, h := range reg.Homes {
		if !aliasRE.MatchString(h.Alias) || !filepath.IsAbs(h.Path) {
			return Registry{}, errorDiag(DiagRegistryInvalid, fmt.Sprintf("%s: entry %q -> %q needs a kebab-case alias and an absolute path", e.RegistryPath(), h.Alias, h.Path), "memo homes register <alias> <path>"), nil
		}
	}
	return reg, nil, nil
}

// Register adds or replaces one alias in homes.json.
func (e Env) Register(alias, path string) (*Diag, error) {
	if !aliasRE.MatchString(alias) {
		return errorDiag(DiagRegistryInvalid, fmt.Sprintf("alias %q must be kebab-case", alias), "memo homes register "+Slugify(alias)+" "+path), nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(filepath.Join(abs, LedgerFile)); err != nil {
		return errorDiag(DiagHomeNotFound, abs+" holds no "+LedgerFile, "memo add <type>/<topic> \"<sentence>.\" --home "+abs), nil
	}
	reg, d, err := e.LoadRegistry()
	if d != nil || err != nil {
		return d, err
	}
	kept := reg.Homes[:0]
	for _, h := range reg.Homes {
		if h.Alias != alias && h.Path != abs {
			kept = append(kept, h)
		}
	}
	reg.Homes = append(kept, RegistryEntry{Alias: alias, Path: abs})
	reg.Version = 1
	if err := os.MkdirAll(filepath.Dir(e.RegistryPath()), 0o755); err != nil {
		return nil, err
	}
	out, _ := json.MarshalIndent(reg, "", "  ")
	return nil, os.WriteFile(e.RegistryPath(), append(out, '\n'), 0o644)
}

// Slugify lowercases a name into a kebab-case alias.
func Slugify(name string) string {
	var b strings.Builder
	dash := true
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			dash = false
		} else if !dash {
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

// ProjectSlug is Claude Code's directory slug for a project path: every
// character outside [A-Za-z0-9] becomes a hyphen.
func ProjectSlug(path string) string {
	var b strings.Builder
	for _, r := range path {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

// RepoRoot is the git toplevel of a directory, or the directory itself.
func RepoRoot(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return filepath.Clean(dir)
	}
	return strings.TrimSpace(string(out))
}

// SessionHomes returns the personal and team home for the cwd, whether or
// not they exist yet. Personal: ~/.claude/projects/<slug of the MAIN
// checkout>/memory (a linked worktree has no personal home of its own);
// team: <this checkout>/.claude/memory, the branch being edited.
func (e Env) SessionHomes() (personal, team Home) {
	repo := RepoRoot(e.Cwd)
	main := MainRepoRoot(repo)
	alias := Slugify(filepath.Base(main))
	personal = Home{Alias: alias, Kind: KindPersonal, Source: "session", Path: filepath.Join(e.UserHome, ".claude", "projects", ProjectSlug(main), "memory")}
	team = Home{Alias: alias + "-team", Kind: KindTeam, Source: "session", Path: filepath.Join(repo, ".claude", "memory")}
	return personal, team
}

// hasLedger reports whether a directory holds a LEDGER.md.
func hasLedger(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, LedgerFile))
	return err == nil
}

// Discover lists every known home: personal homes under ~/.claude/projects
// with a ledger, the team homes their `repo:` points at, the cwd's own
// session homes, and registry entries (which win on alias).
func (e Env) Discover() ([]Home, *Diag, error) {
	reg, d, err := e.LoadRegistry()
	if d != nil || err != nil {
		return nil, d, err
	}
	byPath := map[string]Home{}
	add := func(h Home) {
		if !hasLedger(h.Path) {
			return
		}
		if l, d, err := Load(filepath.Join(h.Path, LedgerFile)); err == nil && d == nil {
			h.Kind = l.Kind
			if h.Source != "registered" && l.Alias != "" {
				h.Alias = l.Alias
			}
			if l.Kind == KindPersonal && l.Repo != "" {
				byPathAdd(byPath, Home{Alias: l.Alias + "-team", Kind: KindTeam, Source: "discovered", Path: filepath.Join(l.Repo, ".claude", "memory")})
			}
		}
		byPathAdd(byPath, h)
	}
	matches, _ := filepath.Glob(filepath.Join(e.UserHome, ".claude", "projects", "*", "memory", LedgerFile))
	for _, m := range matches {
		add(Home{Path: filepath.Dir(m), Kind: KindPersonal, Source: "discovered"})
	}
	personal, team := e.SessionHomes()
	add(personal)
	// Listings, registry and vault name the MAIN checkout's team home even
	// when only this worktree's copy carries a ledger yet (the branch being
	// edited): a symlink into .worktrees/<x> dies with the worktree.
	mainTeam := filepath.Join(MainRepoRoot(RepoRoot(e.Cwd)), ".claude", "memory")
	if hasLedger(team.Path) || hasLedger(mainTeam) {
		team.Source = "session"
		if prev, ok := byPath[filepath.Clean(mainTeam)]; !ok || prev.Source != "registered" {
			team.Path = filepath.Clean(mainTeam)
			byPath[team.Path] = team
		}
	}
	for _, r := range reg.Homes {
		add(Home{Alias: r.Alias, Path: r.Path, Source: "registered"})
	}
	var out []Home
	for _, h := range byPath {
		out = append(out, h)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out, nil, nil
}

func byPathAdd(byPath map[string]Home, h Home) {
	if !hasLedger(h.Path) {
		return
	}
	h.Path = filepath.Clean(h.Path)
	if prev, ok := byPath[h.Path]; ok && prev.Source == "registered" {
		return
	}
	if h.Kind == "" {
		if l, d, err := Load(filepath.Join(h.Path, LedgerFile)); err == nil && d == nil {
			h.Kind = l.Kind
			if h.Source != "registered" {
				h.Alias = l.Alias
			}
		}
	}
	byPath[h.Path] = h
}

// Resolve turns a `--home` value (alias or path, "" for the session default)
// into a home. With "" and a row type, the home owning that type is returned
// (created lazily by the caller); without a type, the session homes that
// exist are returned in personal, team order.
func (e Env) Resolve(spec string, rowType string) ([]Home, *Diag, error) {
	if spec == "" {
		personal, team := e.SessionHomes()
		if rowType != "" {
			if TypeKind[rowType] == KindTeam {
				return []Home{team}, nil, nil
			}
			return []Home{personal}, nil, nil
		}
		var out []Home
		for _, h := range []Home{personal, team} {
			if hasLedger(h.Path) {
				out = append(out, h)
			}
		}
		if len(out) == 0 {
			return nil, errorDiag(DiagHomeNotFound, fmt.Sprintf("no ledger at %s or %s", personal.Path, team.Path), "memo add feedback/<topic> \"<sentence>.\"  # creates the personal ledger; memo homes --json lists every home"), nil
		}
		return out, nil, nil
	}
	if strings.ContainsAny(spec, "/\\") || spec == "." || spec == "~" || isDir(filepath.Join(e.Cwd, spec)) {
		path := spec
		if strings.HasPrefix(path, "~/") {
			path = filepath.Join(e.UserHome, path[2:])
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(e.Cwd, path)
		}
		abs := filepath.Clean(path)
		h := Home{Alias: Slugify(filepath.Base(filepath.Dir(abs))), Path: abs, Source: "path"}
		if personalHomeRE.MatchString(filepath.ToSlash(abs)) {
			h.Kind = KindPersonal
		} else {
			h.Kind = KindTeam
		}
		if hasLedger(abs) {
			if l, d, err := Load(filepath.Join(abs, LedgerFile)); err == nil && d == nil {
				h.Alias, h.Kind = l.Alias, l.Kind
			}
		} else if rowType != "" {
			h.Kind = TypeKind[rowType]
		}
		return []Home{h}, nil, nil
	}
	homes, d, err := e.Discover()
	if d != nil || err != nil {
		return nil, d, err
	}
	for _, h := range homes {
		if h.Alias == spec {
			return []Home{h}, nil, nil
		}
	}
	return nil, errorDiag(DiagHomeNotFound, fmt.Sprintf("no home is aliased %q", spec), "memo homes --json  # or: memo homes register "+spec+" <path>"), nil
}

// Open loads the ledger of a home, creating it when create is set. Alias and
// kind for a new ledger come from the home; repo from the session cwd for a
// personal home.
func (e Env) Open(h Home, create bool) (*Ledger, *Diag, error) {
	path := filepath.Join(h.Path, LedgerFile)
	l, d, err := Load(path)
	if d != nil {
		return nil, d, nil
	}
	if err == nil {
		return l, nil, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, nil, err
	}
	if !create {
		return nil, errorDiag(DiagHomeNotFound, "no "+LedgerFile+" in "+h.Path, "memo add <type>/<topic> \"<sentence>.\" --home "+h.Path+"  # creates it"), nil
	}
	repo := ""
	alias := h.Alias
	if h.Kind == KindPersonal {
		if repo = e.personalRepo(h.Path); repo != "" {
			alias = Slugify(filepath.Base(repo))
		}
	} else if root, ok := gitToplevel(h.Path); ok {
		alias = Slugify(filepath.Base(MainRepoRoot(root))) + "-team"
	}
	l, err = Create(path, alias, h.Kind, repo)
	return l, nil, err
}

// Ref is a parsed row reference: an optional alias plus an id.
type Ref struct {
	Alias string
	ID    int
	Team  bool // #t12 / ^t12: the team ledger's id space
}

var refRE = regexp.MustCompile(`^(?:#?(t?)(\d+)|([a-z0-9]+(?:-[a-z0-9]+)*)#(t?)(\d+)|\^([mt])(\d+)|\[\[(?:([a-z0-9]+(?:-[a-z0-9]+)*)/)?LEDGER#\^([mt])(\d+)\]\])$`)

// ParseRef accepts `45`, `#45`, `^m45` (personal), `t45`, `#t45`, `^t45`
// (team), `fixit-team#12` and wikilinks.
func ParseRef(s string) (Ref, *Diag) {
	m := refRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return Ref{}, errorDiag(DiagRefSyntax, fmt.Sprintf("%q is not a row reference (45, #45, t12, #t12, fixit-team#12, [[LEDGER#^m45]])", s), "memo show 45")
	}
	for _, tri := range [][3]string{{"", m[1], m[2]}, {m[3], m[4], m[5]}, {"", m[6], m[7]}, {m[8], m[9], m[10]}} {
		if tri[2] != "" {
			r := Ref{Alias: tri[0], Team: tri[1] == "t"}
			fmt.Sscanf(tri[2], "%d", &r.ID)
			return r, nil
		}
	}
	return Ref{}, errorDiag(DiagRefSyntax, "empty reference", "memo show 45")
}

// Locate resolves a ref to the ledger holding it. A bare id is looked up in
// the `--home` home when given, else in every session home; ambiguity is a
// diagnostic naming the alias forms.
func (e Env) Locate(ref Ref, homeSpec string) (*Ledger, Row, *Diag, error) {
	spec := homeSpec
	if ref.Alias != "" {
		spec = ref.Alias
	}
	homes, d, err := e.Resolve(spec, "")
	if d != nil || err != nil {
		return nil, Row{}, d, err
	}
	if spec == "" { // a bare id names the personal ledger, a t-id the team ledger
		var kept []Home
		for _, h := range homes {
			if (h.Kind == KindTeam) == ref.Team {
				kept = append(kept, h)
			}
		}
		homes = kept
	}
	var hits []*Ledger
	var rows []Row
	for _, h := range homes {
		l, d, err := e.Open(h, false)
		if err != nil {
			return nil, Row{}, nil, err
		}
		if d != nil {
			continue
		}
		if r, ok := l.Find(ref.ID); ok {
			hits, rows = append(hits, l), append(rows, r)
		}
	}
	switch len(hits) {
	case 0:
		return nil, Row{}, errorDiag(DiagRefUnknown, fmt.Sprintf("no row #%d in %s", ref.ID, homeNames(homes)), "memo find <words> --json"), nil
	case 1:
		return hits[0], rows[0], nil, nil
	}
	return nil, Row{}, errorDiag(DiagRefAmbiguous, fmt.Sprintf("#%d exists in %s and %s", ref.ID, hits[0].Alias, hits[1].Alias), fmt.Sprintf("memo show %s#%d", hits[0].Alias, ref.ID)), nil
}

func homeNames(homes []Home) string {
	var names []string
	for _, h := range homes {
		names = append(names, h.Alias)
	}
	return strings.Join(names, ", ")
}

// isDir reports whether a `--home` argument names an existing directory; such
// an argument is a path, never an alias.
func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// Enclosing returns the discovered or registered home a path lies inside.
func (e Env) Enclosing(path string) (Home, bool) {
	homes, d, err := e.Discover()
	if d != nil || err != nil {
		return Home{}, false
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return Home{}, false
	}
	for _, h := range homes {
		if abs == h.Path || strings.HasPrefix(abs, h.Path+string(filepath.Separator)) {
			return h, true
		}
	}
	return Home{}, false
}

// gitToplevel is the work tree containing dir (or its nearest existing
// ancestor, so a home that is about to be created still resolves), if any.
func gitToplevel(dir string) (string, bool) {
	for !isDir(dir) {
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// MainRepoRoot is the MAIN work tree of the repository containing dir (the
// first entry of `git worktree list`), so a home inside `.worktrees/<x>`
// still derives its alias from the repo's basename. Outside git it is the
// directory itself.
func MainRepoRoot(dir string) string {
	root, ok := gitToplevel(dir)
	if !ok {
		return filepath.Clean(dir)
	}
	out, err := exec.Command("git", "-C", root, "worktree", "list", "--porcelain").Output()
	if err == nil {
		for _, line := range strings.Split(string(out), "\n") {
			if strings.HasPrefix(line, "worktree ") {
				return strings.TrimPrefix(line, "worktree ")
			}
		}
	}
	return root
}

// MainCheckoutTeamHome refuses a team home that lives in a repository's MAIN
// checkout: a row appended there is uncommitted dirt on the default branch,
// which changes reach only through a worktree and a PR. A home outside git,
// in a linked worktree, or in a repo with WORKTREE_POLICY=never passes.
// branch names the worktree the fix proposes.
func MainCheckoutTeamHome(home, branch string) (*Diag, error) {
	root, ok := gitToplevel(home)
	if !ok || root != MainRepoRoot(root) {
		return nil, nil
	}
	cfg, err := gitkit.ReadGitConfig(root)
	if err != nil {
		return nil, err
	}
	if cfg["WORKTREE_POLICY"] == "never" {
		return nil, nil
	}
	wt := filepath.Join(root, ".worktrees", Slugify(filepath.Base(branch)))
	return errorDiag(DiagMainCheckout, home+" is in the main checkout of "+root+", so the row was not written: a team row there is uncommitted dirt on the default branch; add it from a worktree and land it through a PR (WORKTREE_POLICY=never in .claude/.claude.git.config opts a repo out)", "git -C "+shellword.Quote(root)+" worktree add -b "+shellword.Quote(branch)+" "+shellword.Quote(wt)+" && cd "+shellword.Quote(wt)+"  # then re-run this memo command"), nil
}

// SetAlias rewrites the `alias:` frontmatter line of a ledger; the one
// sanctioned edit of LEDGER.md outside an append.
func SetAlias(path, alias string) (*Diag, error) {
	if !aliasRE.MatchString(alias) {
		return errorDiag(DiagRegistryInvalid, fmt.Sprintf("alias %q must be kebab-case", alias), "memo homes alias "+path+" "+Slugify(alias)), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.SplitN(string(data), "\n", 8)
	for i, line := range lines {
		if line == "---" && i > 0 {
			break
		}
		if strings.HasPrefix(line, "alias:") {
			lines[i] = "alias: " + alias
			return nil, os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)
		}
	}
	return errorDiag(DiagLedgerInvalid, path+" has no alias: line in its frontmatter", "memorylint check "+filepath.Dir(path)), nil
}

// personalRepo finds the repo a personal home stands for: the cwd's main
// work tree when the home's slug is that checkout's, else the slug decoded
// back to an existing path by walking the filesystem, else "" so the
// caller keeps the directory-derived alias.
func (e Env) personalRepo(home string) string {
	slug := filepath.Base(filepath.Dir(home))
	if !personalHomeRE.MatchString(filepath.ToSlash(home)) {
		return MainRepoRoot(e.Cwd)
	}
	if checkout := RepoRoot(e.Cwd); ProjectSlug(checkout) == slug {
		return MainRepoRoot(checkout)
	}
	if path, ok := decodeSlug("/", strings.TrimPrefix(slug, "-"), 0); ok {
		return MainRepoRoot(path)
	}
	return ""
}

// decodeSlug walks the real filesystem to find the path a Claude Code slug
// encodes: at each directory it descends into the entry whose own slug is
// the next dash-terminated prefix of what remains. The slug is lossy (`-`,
// `_`, `.` all become `-`), so only the filesystem can disambiguate.
func decodeSlug(dir, rest string, depth int) (string, bool) {
	if rest == "" {
		return dir, true
	}
	if depth > 32 {
		return "", false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if !isDir(filepath.Join(dir, e.Name())) { // Stat, not DirEntry: /var -> /private/var is a symlink
			continue
		}
		name := ProjectSlug(e.Name())
		switch {
		case rest == name:
			return filepath.Join(dir, e.Name()), true
		case strings.HasPrefix(rest, name+"-"):
			if path, ok := decodeSlug(filepath.Join(dir, e.Name()), rest[len(name)+1:], depth+1); ok {
				return path, true
			}
		}
	}
	return "", false
}

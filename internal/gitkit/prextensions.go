package gitkit

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// pr-extensions — list the prm extensions a repo ships: markdown instruction
// files prm reads and follows at a fixed point of its flow. The shell hooks
// (BEFORE_REVIEW_CMD, AFTER_MERGE_CMD) cannot carry a step that needs the
// model — drafting release-note copy from the diff when the PR opens — so an
// extension is prose, and this verb only finds and validates it.
//
// `pr-extensions [--stage ensure-pr|round|merge] [--repo <abs>]`.
//
// Only merged content runs. prm follows an extension with the session's full
// tool access, so the key and the files are read as git BLOBS at
// origin/<default branch> — never a working tree (a PR branch checked out in
// the main clone, an uncommitted edit) and never a symlink (it points outside
// review): anything else would let a PR, a foreign one included, write the
// steps prm then executes on it. The default branch is DEFAULT_BRANCH from the
// gitignored .local, else the one committed at origin/HEAD, else origin/HEAD;
// the instructions travel in the output, so prm follows exactly what was
// validated. The cost is that the PR adding an extension does not run it. A
// key set to a glob that matches nothing, or a malformed file, exits 1 — a
// repo that ships an extension expects it to run, so it is never skipped.

// PRExtensionStages are the points of prm's flow an extension can hook, in
// flow order.
var PRExtensionStages = []string{"ensure-pr", "round", "merge"}

// PRExtension is one validated extension file, keys in wire order.
type PRExtension struct {
	Name         string   `json:"name"`
	Stages       []string `json:"stages"`
	Description  string   `json:"description"`
	RelPath      string   `json:"relPath"`
	Instructions string   `json:"instructions"`
}

// PRExtensions is pr-extensions' output, keys in wire order. PRExtensions is
// the configured glob (null: unset or empty — no extensions); Stage is the
// --stage filter (null: every extension); Ref and Commit name the merged
// content everything was read from.
type PRExtensions struct {
	PRExtensions *string       `json:"prExtensions"`
	Stage        *string       `json:"stage"`
	Ref          string        `json:"ref"`
	Commit       string        `json:"commit"`
	Extensions   []PRExtension `json:"extensions"`
}

// treeEntry is one `git ls-tree -r` line.
type treeEntry struct{ mode, oid, path string }

func parseTree(listing string) []treeEntry {
	out := []treeEntry{}
	for rec := range strings.SplitSeq(strings.TrimSuffix(listing, "\x00"), "\x00") {
		meta, path, ok := strings.Cut(rec, "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			continue
		}
		out = append(out, treeEntry{mode: fields[0], oid: fields[2], path: path})
	}
	return out
}

var extensionName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// stageList accepts `stage: round` as well as `stage: [ensure-pr, round]`.
type stageList []string

func (s *stageList) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		*s = stageList{n.Value}
		return nil
	case yaml.SequenceNode:
		var v []string
		if err := n.Decode(&v); err != nil {
			return err
		}
		*s = v
		return nil
	}
	return errors.New("stage must be a stage name or a list of them")
}

type extensionFrontmatter struct {
	Name        string    `yaml:"name"`
	Stage       stageList `yaml:"stage"`
	Description string    `yaml:"description"`
	Body        string    `yaml:"-"`
}

// parseExtension validates one extension file's frontmatter and body.
func parseExtension(data []byte) (extensionFrontmatter, error) {
	var fm extensionFrontmatter
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return fm, errors.New("no frontmatter: the file must start with a --- line")
	}
	head, body, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		head, ok = strings.CutSuffix(rest, "\n---")
	}
	if !ok {
		return fm, errors.New("frontmatter is not closed by a --- line")
	}
	dec := yaml.NewDecoder(bytes.NewReader([]byte(head)))
	dec.KnownFields(true) // a misspelt key must not rot silently
	if err := dec.Decode(&fm); err != nil {
		return fm, fmt.Errorf("frontmatter: %w", err)
	}
	fm.Name, fm.Description = strings.TrimSpace(fm.Name), strings.TrimSpace(fm.Description)
	switch {
	case !extensionName.MatchString(fm.Name):
		return fm, fmt.Errorf("name %q must be kebab-case", fm.Name)
	case fm.Description == "" || strings.Contains(fm.Description, "\n"):
		return fm, errors.New("description must be one non-empty line")
	case len(fm.Stage) == 0:
		return fm, fmt.Errorf("stage is required (one or more of %s)", strings.Join(PRExtensionStages, ", "))
	case strings.TrimSpace(body) == "":
		return fm, errors.New("has no instructions below the frontmatter")
	}
	for i, st := range fm.Stage {
		if !slices.Contains(PRExtensionStages, st) {
			return fm, fmt.Errorf("unknown stage %q (one of %s)", st, strings.Join(PRExtensionStages, ", "))
		}
		if slices.Contains(fm.Stage[:i], st) {
			return fm, fmt.Errorf("stage %q is listed twice", st)
		}
	}
	fm.Body = strings.TrimSpace(body)
	return fm, nil
}

// resolvePRExtensions matches glob against the merged tree and validates
// every match; read returns a blob's content.
func resolvePRExtensions(glob string, tree []treeEntry, read func(oid string) (string, error)) ([]PRExtension, error) {
	clean := strings.TrimPrefix(glob, "./")
	if filepath.IsAbs(clean) || slices.Contains(strings.Split(clean, "/"), "..") {
		return nil, fmt.Errorf("PR_EXTENSIONS=%s must be a glob relative to the repository root", glob)
	}
	re := globToRegExp(clean)
	out := []PRExtension{}
	for _, e := range tree {
		if !re.MatchString(e.path) {
			continue
		}
		if e.mode != "100644" && e.mode != "100755" {
			return nil, fmt.Errorf("%s: must be a regular file, not mode %s — a symlink's target is outside review", e.path, e.mode)
		}
		data, err := read(e.oid)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.path, err)
		}
		fm, err := parseExtension([]byte(data))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", e.path, err)
		}
		for _, prev := range out {
			if prev.Name == fm.Name {
				return nil, fmt.Errorf("%s: name %q is already used by %s", e.path, fm.Name, prev.RelPath)
			}
		}
		out = append(out, PRExtension{Name: fm.Name, Stages: fm.Stage, Description: fm.Description, RelPath: e.path, Instructions: fm.Body})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("PR_EXTENSIONS=%s matches no file", glob)
	}
	slices.SortFunc(out, func(a, b PRExtension) int { return strings.Compare(a.RelPath, b.RelPath) })
	return out, nil
}

func runPRExtensions(args []string, stdout, stderr io.Writer) int {
	var stage *string
	if i := slices.Index(args, "--stage"); i != -1 {
		next := ""
		if i+1 < len(args) {
			next = args[i+1]
		}
		if !slices.Contains(PRExtensionStages, next) {
			return fail(stderr, fmt.Errorf("--stage must be one of %s", strings.Join(PRExtensionStages, ", ")))
		}
		stage = &next
	}
	root, err := repoRoot(args)
	if err != nil {
		return fail(stderr, err)
	}
	git := func(a ...string) (string, error) {
		return execFile(execOpts{dir: root, echo: stderr, timeout: 30 * time.Second, maxBuffer: 64 << 20}, "git", a...)
	}
	list, err := git("worktree", "list", "--porcelain")
	if err != nil {
		return fail(stderr, err)
	}
	mainClone := mainCloneOf(parseWorktreeList(list))
	if mainClone == "" {
		return fail(stderr, errors.New("could not determine the main clone from `git worktree list`"))
	}
	// .local is the machine's own gitignored file; everything else is read
	// at the merged ref.
	localData, _, err := readIfPresent(filepath.Join(mainClone, ".claude/.claude.git.config.local"))
	if err != nil {
		return fail(stderr, err)
	}
	local := parseConfig(string(localData))
	readAt := func(branch string) (commit string, committed gitConfig, tree []treeEntry, err error) {
		ref := "origin/" + branch
		if commit, err = git("rev-parse", "--verify", "-q", ref+"^{commit}"); err != nil {
			return "", nil, nil, fmt.Errorf("%s does not exist — run `git fetch origin %s`", ref, branch)
		}
		listing, err := git("ls-tree", "-r", "-z", "--full-tree", ref)
		if err != nil {
			return "", nil, nil, err
		}
		tree, committed = parseTree(listing), gitConfig{}
		for _, e := range tree {
			if e.path == ".claude/.claude.git.config" {
				data, err := git("cat-file", "blob", e.oid)
				if err != nil {
					return "", nil, nil, err
				}
				committed = parseConfig(data)
			}
		}
		return strings.TrimSpace(commit), committed, tree, nil
	}
	branch, pinned := local.get("DEFAULT_BRANCH")
	if !pinned || branch == "" {
		head, err := git("symbolic-ref", "-q", "--short", "refs/remotes/origin/HEAD")
		if err != nil {
			return fail(stderr, errors.New("cannot tell the default branch: origin/HEAD is unset — run `git remote set-head origin --auto`, or set DEFAULT_BRANCH in .claude/.claude.git.config.local"))
		}
		branch, pinned = strings.TrimPrefix(strings.TrimSpace(head), "origin/"), false
	}
	commit, cfg, tree, err := readAt(branch)
	if err != nil {
		return fail(stderr, err)
	}
	if committedBranch, _ := cfg.get("DEFAULT_BRANCH"); !pinned && committedBranch != "" && committedBranch != branch {
		branch = committedBranch
		if commit, cfg, tree, err = readAt(branch); err != nil {
			return fail(stderr, err)
		}
	}
	for k, v := range local {
		cfg[k] = v
	}

	out := PRExtensions{Stage: stage, Ref: "origin/" + branch, Commit: commit, Extensions: []PRExtension{}}
	if glob, _ := cfg.get("PR_EXTENSIONS"); glob != "" {
		out.PRExtensions = &glob
		all, err := resolvePRExtensions(glob, tree, func(oid string) (string, error) { return git("cat-file", "blob", oid) })
		if err != nil {
			return fail(stderr, fmt.Errorf("%s: %w", out.Ref, err))
		}
		for _, ext := range all {
			if stage == nil || slices.Contains(ext.Stages, *stage) {
				out.Extensions = append(out.Extensions, ext)
			}
		}
	}
	if err := writeJSON(stdout, out); err != nil {
		return fail(stderr, err)
	}
	return 0
}

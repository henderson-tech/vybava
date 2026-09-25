package gitkit

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
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
// PR_EXTENSIONS=<glob> is read like every other key (main clone, .local
// overlay) and the glob matches the files git TRACKS in the main clone —
// never a PR branch's copy. prm follows an extension with the session's full
// tool access, so its instructions must be reviewed, merged content: reading
// the branch would let any PR, a foreign one included, write the steps prm
// then executes on it. The cost is that the PR adding an extension does not
// run it. A key set to a glob that matches nothing, or a malformed file,
// exits 1 — a repo that ships an extension expects it to run, so it is never
// skipped quietly.

// PRExtensionStages are the points of prm's flow an extension can hook, in
// flow order.
var PRExtensionStages = []string{"ensure-pr", "round", "merge"}

// PRExtension is one validated extension file, keys in wire order.
type PRExtension struct {
	Name        string   `json:"name"`
	Stages      []string `json:"stages"`
	Description string   `json:"description"`
	Path        string   `json:"path"`
	RelPath     string   `json:"relPath"`
}

// PRExtensions is pr-extensions' output, keys in wire order. PRExtensions is
// the configured glob (null: unset or empty — no extensions); Stage is the
// --stage filter (null: every extension). Paths point into MainClone.
type PRExtensions struct {
	ConfigFound  bool          `json:"configFound"`
	ConfigPath   string        `json:"configPath"`
	PRExtensions *string       `json:"prExtensions"`
	Stage        *string       `json:"stage"`
	MainClone    string        `json:"mainClone"`
	Extensions   []PRExtension `json:"extensions"`
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
	return fm, nil
}

// resolvePRExtensions matches glob against the main clone's tracked files
// and validates every match.
func resolvePRExtensions(glob string, files []string, root string) ([]PRExtension, error) {
	clean := strings.TrimPrefix(glob, "./")
	if filepath.IsAbs(clean) || slices.Contains(strings.Split(clean, "/"), "..") {
		return nil, fmt.Errorf("PR_EXTENSIONS=%s must be a glob relative to the repository root", glob)
	}
	re := globToRegExp(clean)
	out := []PRExtension{}
	for _, rel := range files {
		if !re.MatchString(rel) {
			continue
		}
		path := filepath.Join(root, rel)
		data, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue // tracked but deleted in the working tree
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		fm, err := parseExtension(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		for _, prev := range out {
			if prev.Name == fm.Name {
				return nil, fmt.Errorf("%s: name %q is already used by %s", rel, fm.Name, prev.RelPath)
			}
		}
		out = append(out, PRExtension{Name: fm.Name, Stages: fm.Stage, Description: fm.Description, Path: path, RelPath: rel})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("PR_EXTENSIONS=%s matches no file in %s", glob, root)
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
	git := func(dir string, a ...string) (string, error) {
		return execFile(execOpts{dir: dir, echo: stderr, timeout: 30 * time.Second, maxBuffer: 32 << 20}, "git", a...)
	}
	list, err := git(root, "worktree", "list", "--porcelain")
	if err != nil {
		return fail(stderr, err)
	}
	mainClone := mainCloneOf(parseWorktreeList(list))
	if mainClone == "" {
		return fail(stderr, errors.New("could not determine the main clone from `git worktree list`"))
	}
	cfg, configFound, err := readGitConfig(mainClone)
	if err != nil {
		return fail(stderr, err)
	}

	out := PRExtensions{
		ConfigFound: configFound,
		ConfigPath:  filepath.Join(mainClone, ".claude/.claude.git.config"),
		Stage:       stage,
		MainClone:   mainClone,
		Extensions:  []PRExtension{},
	}
	if glob, _ := cfg.get("PR_EXTENSIONS"); glob != "" {
		out.PRExtensions = &glob
		listing, err := git(mainClone, "ls-files", "-z")
		if err != nil {
			return fail(stderr, err)
		}
		files := strings.Split(strings.TrimSuffix(listing, "\x00"), "\x00")
		all, err := resolvePRExtensions(glob, files, mainClone)
		if err != nil {
			return fail(stderr, err)
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

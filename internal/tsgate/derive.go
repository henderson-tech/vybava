package tsgate

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Plan is what tsgate runs for one program: the leaf tsconfig, and when TS 7
// refuses its chain, the derived config it writes beside the leaf instead.
type Plan struct {
	Tsconfig string   `json:"tsconfig"`
	Derive   bool     `json:"derive"`
	Reasons  []string `json:"reasons"`
	Target   string   `json:"target"` // the config the compiler reads
	Config   *Derived `json:"config,omitempty"`
}

// Derived is the flattened TS 7 config.
type Derived struct {
	Comment         string                     `json:"//"`
	CompilerOptions map[string]json.RawMessage `json:"compilerOptions"`
	Include         *[]string                  `json:"include,omitempty"`
	Exclude         *[]string                  `json:"exclude,omitempty"`
	Files           *[]string                  `json:"files,omitempty"`
}

// pathOptions hold one path, resolved against the config that declares them.
var pathOptions = map[string]bool{"rootDir": true, "outDir": true, "declarationDir": true, "outFile": true, "tsBuildInfoFile": true}

// pathListOptions hold a list of paths, resolved the same way.
var pathListOptions = map[string]bool{"typeRoots": true, "rootDirs": true}

// droppedOptions leave a derived config: a cold TS 7 check is seconds, its
// build info must never overwrite the TS 6 one, and TS 7 has no TS 6
// deprecations left to silence.
var droppedOptions = []string{"ignoreDeprecations", "incremental", "tsBuildInfoFile"}

// PlanProgram reads the chain ending at tsconfig and decides whether TS 7 can
// read it as it is. It refuses it when the chain sets:
//   - baseUrl (removed): it becomes the `"*"` paths mapping TS documents as its
//     equivalent, and every paths target is re-anchored;
//   - moduleResolution node/node10 (removed): module preserve + moduleResolution
//     bundler, with package.json `exports` resolution off as node10 had it, so
//     deep imports into exports-mapped packages still resolve;
//   - outDir without rootDir: TS 7 then takes rootDir as the config's own
//     directory and refuses every file the program reaches outside it (TS6059);
//     rootDir becomes the checkout root.
//
// Anything else TS 7 removed (target ES5, esModuleInterop false, module amd,
// …) is left to TS 7's own TS5102/TS5108 message: it has no equivalent to
// rewrite to, so the real tsconfig has to move.
func PlanProgram(tsconfig string) (Plan, error) {
	leaf, err := filepath.Abs(tsconfig)
	if err != nil {
		return Plan{}, err
	}
	chain, err := loadChain(leaf)
	if err != nil {
		return Plan{}, err
	}
	m, err := mergeChain(chain, filepath.Dir(leaf))
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{Tsconfig: leaf, Target: leaf, Reasons: []string{}}
	resolution := strings.ToLower(m.stringOption("moduleResolution"))
	node10 := resolution == "node" || resolution == "node10"
	if m.baseURL != "" {
		plan.Reasons = append(plan.Reasons, "baseUrl is removed in TS 7: it becomes the \"*\" paths mapping")
	}
	if node10 {
		plan.Reasons = append(plan.Reasons, fmt.Sprintf("moduleResolution %s is removed in TS 7: bundler + module preserve, package.json exports off as %s had it", resolution, resolution))
	}
	_, hasOutDir := m.paths["outDir"]
	_, hasRootDir := m.paths["rootDir"]
	if hasOutDir && !hasRootDir {
		plan.Reasons = append(plan.Reasons, "outDir without rootDir: TS 7 takes rootDir as the config's directory; it becomes the checkout root")
	}
	if len(plan.Reasons) == 0 {
		return plan, nil
	}
	plan.Derive = true
	plan.Target = derivedPath(leaf)
	plan.Config = m.derive(leaf, node10)
	return plan, nil
}

// derivedPath is `.tsconfig.spec.ts7.json` beside `tsconfig.spec.json`.
func derivedPath(leaf string) string {
	return filepath.Join(filepath.Dir(leaf), "."+strings.TrimSuffix(filepath.Base(leaf), ".json")+".ts7.json")
}

// merged is a chain after `extends`: option values as written, every path
// absolute.
type merged struct {
	options   map[string]json.RawMessage
	paths     map[string]string   // pathOptions
	pathLists map[string][]string // pathListOptions
	baseURL   string
	mapping   map[string][]string // compilerOptions.paths
	mapDir    string              // the config that declared it
	include   *[]string
	exclude   *[]string
	files     *[]string
}

func (m merged) stringOption(key string) string {
	var s string
	_ = json.Unmarshal(m.options[key], &s)
	return s
}

// mergeChain applies the layers base first: a later layer's option wins, a
// `null` resets it, include/exclude/files and paths are replaced whole, and a
// path is anchored to the config that declares it (`${configDir}` to the leaf).
func mergeChain(chain []layer, leafDir string) (merged, error) {
	m := merged{options: map[string]json.RawMessage{}, paths: map[string]string{}, pathLists: map[string][]string{}}
	anchor := func(dir, p string) string {
		if rest, ok := strings.CutPrefix(p, "${configDir}"); ok {
			return filepath.Join(leafDir, rest)
		}
		if filepath.IsAbs(p) {
			return filepath.Clean(p)
		}
		return filepath.Join(dir, p)
	}
	anchorList := func(dir string, list *[]string) *[]string {
		if list == nil {
			return nil
		}
		out := make([]string, len(*list))
		for i, p := range *list {
			out[i] = anchor(dir, p)
		}
		return &out
	}
	for _, l := range chain {
		for key, raw := range l.options {
			if string(raw) == "null" {
				delete(m.options, key)
				delete(m.paths, key)
				delete(m.pathLists, key)
				if key == "baseUrl" {
					m.baseURL = ""
				}
				if key == "paths" {
					m.mapping = nil
				}
				continue
			}
			switch {
			case key == "baseUrl":
				var s string
				if err := json.Unmarshal(raw, &s); err != nil {
					return m, fmt.Errorf("%s: \"baseUrl\" must be a string", l.file)
				}
				m.baseURL = anchor(l.dir, s)
			case key == "paths":
				var mapping map[string][]string
				if err := json.Unmarshal(raw, &mapping); err != nil {
					return m, fmt.Errorf("%s: \"paths\" must map patterns to lists of strings", l.file)
				}
				m.mapping, m.mapDir = mapping, l.dir
			case pathOptions[key]:
				var s string
				if err := json.Unmarshal(raw, &s); err != nil {
					return m, fmt.Errorf("%s: %q must be a string", l.file, key)
				}
				m.paths[key] = anchor(l.dir, s)
			case pathListOptions[key]:
				var list []string
				if err := json.Unmarshal(raw, &list); err != nil {
					return m, fmt.Errorf("%s: %q must be a list of strings", l.file, key)
				}
				m.pathLists[key] = *anchorList(l.dir, &list)
			default:
				m.options[key] = raw
			}
		}
		if l.include != nil {
			m.include = anchorList(l.dir, l.include)
		}
		if l.exclude != nil {
			m.exclude = anchorList(l.dir, l.exclude)
		}
		if l.files != nil {
			m.files = anchorList(l.dir, l.files)
		}
	}
	return m, nil
}

// derive writes the merged chain as one config for a file beside the leaf.
func (m merged) derive(leaf string, node10 bool) *Derived {
	out := filepath.Dir(leaf)
	opts := map[string]json.RawMessage{}
	for k, v := range m.options {
		opts[k] = v
	}
	str := func(s string) json.RawMessage {
		raw, _ := json.Marshal(s)
		return raw
	}
	for k, p := range m.paths {
		opts[k] = str(relativeTo(out, p))
	}
	for k, list := range m.pathLists {
		rel := make([]string, len(list))
		for i, p := range list {
			rel[i] = relativeTo(out, p)
		}
		raw, _ := json.Marshal(rel)
		opts[k] = raw
	}
	for _, k := range droppedOptions {
		delete(opts, k)
	}
	if node10 {
		opts["module"] = str("preserve")
		opts["moduleResolution"] = str("bundler")
		opts["resolvePackageJsonExports"] = json.RawMessage("false")
	}
	if _, ok := m.paths["rootDir"]; !ok {
		if root := checkoutRoot(out); root != "" {
			opts["rootDir"] = str(relativeTo(out, root))
		}
	}
	// Targets resolve against baseUrl when one is set, else the declaring config.
	base := m.baseURL
	if base == "" {
		base = m.mapDir
	}
	mapping := map[string][]string{}
	for pattern, targets := range m.mapping {
		rel := make([]string, len(targets))
		for i, t := range targets {
			if rest, ok := strings.CutPrefix(t, "${configDir}"); ok {
				rel[i] = relativeTo(out, filepath.Join(out, rest))
			} else {
				rel[i] = relativeTo(out, filepath.Join(base, t))
			}
		}
		mapping[pattern] = rel
	}
	if _, ok := mapping["*"]; m.baseURL != "" && !ok {
		mapping["*"] = []string{relativeTo(out, m.baseURL) + "/*"}
	}
	if len(mapping) > 0 {
		raw, _ := json.Marshal(mapping)
		opts["paths"] = raw
	}
	relList := func(list *[]string) *[]string {
		if list == nil {
			return nil
		}
		rel := make([]string, len(*list))
		for i, p := range *list {
			rel[i] = relativeTo(out, p)
		}
		return &rel
	}
	return &Derived{
		Comment:         "Rewritten from " + filepath.Base(leaf) + " by vybava tsgate on every run: edit that.",
		CompilerOptions: opts,
		Include:         relList(m.include),
		Exclude:         relList(m.exclude),
		Files:           relList(m.files),
	}
}

// relativeTo is to as seen from dir, always with the leading `./` or `../`
// TS 7 requires of a relative path.
func relativeTo(dir, to string) string {
	rel, err := filepath.Rel(dir, to)
	if err != nil {
		return filepath.ToSlash(to)
	}
	rel = filepath.ToSlash(rel)
	switch {
	case rel == ".":
		return "."
	case rel == ".." || strings.HasPrefix(rel, "../"):
		return rel
	default:
		return "./" + rel
	}
}

// writeDerived writes the plan's derived config whole: two runs of the same
// program may overlap, so each writes a temp file and renames it into place.
func writeDerived(plan Plan) error {
	body, err := json.MarshalIndent(plan.Config, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", plan.Target, os.Getpid())
	if err := os.WriteFile(tmp, append(body, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, plan.Target)
}

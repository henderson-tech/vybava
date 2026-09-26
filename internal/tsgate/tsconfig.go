// Package tsgate typechecks a TypeScript program on the TypeScript 7 native
// compiler while the repo's `typescript` dependency stays on 6 (or 5.9) for
// every tool that imports the classic compiler API, which TS 7 does not ship
// (ts-node, jest config loaders, typescript-eslint, declaration builders).
//
// TS 7 arrives as an alias devDependency (`"typescript7":
// "npm:typescript@7.0.2"`); its one bin is also named `tsc`, so the
// node_modules/.bin link cannot be trusted and tsgate resolves the alias and
// execs its native binary itself. A tsconfig chain TS 7 refuses (baseUrl,
// moduleResolution node10) is flattened into a derived config beside the leaf
// on every run, so it cannot drift. Contract: docs/tsgate.md.
package tsgate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// layer is one tsconfig file of a chain.
type layer struct {
	file       string
	dir        string
	options    map[string]json.RawMessage
	include    *[]string
	exclude    *[]string
	files      *[]string
	references json.RawMessage // never inherited: only the leaf's count
}

type rawConfig struct {
	Extends         json.RawMessage            `json:"extends"`
	CompilerOptions map[string]json.RawMessage `json:"compilerOptions"`
	Include         *[]string                  `json:"include"`
	Exclude         *[]string                  `json:"exclude"`
	Files           *[]string                  `json:"files"`
	References      json.RawMessage            `json:"references"`
}

// maxChain bounds `extends` depth, so a cycle is an error, not a hang.
const maxChain = 32

// loadChain returns the config and everything it extends, base first, leaf
// last, the way `extends` merges them.
func loadChain(file string) ([]layer, error) {
	return loadChainDepth(file, 0)
}

func loadChainDepth(file string, depth int) ([]layer, error) {
	if depth > maxChain {
		return nil, fmt.Errorf("%s: extends chain deeper than %d (a cycle?)", file, maxChain)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	var cfg rawConfig
	if err := json.Unmarshal(stripJSONC(raw), &cfg); err != nil {
		return nil, fmt.Errorf("%s: %w", file, err)
	}
	var parents []string
	if len(cfg.Extends) > 0 && string(cfg.Extends) != "null" {
		var one string
		if json.Unmarshal(cfg.Extends, &one) == nil {
			parents = []string{one}
		} else if err := json.Unmarshal(cfg.Extends, &parents); err != nil {
			return nil, fmt.Errorf("%s: \"extends\" must be a string or a list of strings", file)
		}
	}
	var chain []layer
	for _, spec := range parents {
		parent, err := resolveExtends(file, spec)
		if err != nil {
			return nil, err
		}
		up, err := loadChainDepth(parent, depth+1)
		if err != nil {
			return nil, err
		}
		chain = append(chain, up...)
	}
	return append(chain, layer{
		file:       file,
		dir:        filepath.Dir(file),
		options:    cfg.CompilerOptions,
		include:    cfg.Include,
		exclude:    cfg.Exclude,
		files:      cfg.Files,
		references: cfg.References,
	}), nil
}

// resolveExtends finds an `extends` target: a path relative to the declaring
// config, or a package specifier (`expo/tsconfig.base`,
// `@tsconfig/node20/tsconfig.json`) resolved through node_modules.
func resolveExtends(from, spec string) (string, error) {
	if strings.HasPrefix(spec, ".") || filepath.IsAbs(spec) {
		target := spec
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(from), spec)
		}
		for _, candidate := range []string{target, target + ".json"} {
			if isFile(candidate) {
				return candidate, nil
			}
		}
		return "", fmt.Errorf("%s: extends %q: no such file", from, spec)
	}
	name, sub := splitPackage(spec)
	for dir := filepath.Dir(from); ; dir = filepath.Dir(dir) {
		pkg := filepath.Join(dir, "node_modules", name)
		if isDir(pkg) {
			if target, ok := resolveInPackage(pkg, sub); ok {
				return target, nil
			}
			return "", fmt.Errorf("%s: extends %q: %s has no such config", from, spec, pkg)
		}
		if filepath.Dir(dir) == dir {
			return "", fmt.Errorf("%s: extends %q: package %s is not installed", from, spec, name)
		}
	}
}

// splitPackage splits `@scope/pkg/sub/path` into the package and its subpath.
func splitPackage(spec string) (name, sub string) {
	parts := strings.Split(spec, "/")
	n := 1
	if strings.HasPrefix(spec, "@") && len(parts) > 1 {
		n = 2
	}
	return strings.Join(parts[:n], "/"), strings.Join(parts[n:], "/")
}

// resolveInPackage finds a config inside an installed package: its
// package.json `tsconfig` field or tsconfig.json for a bare name, else the
// `exports` entry for the subpath, else the subpath as a file, with `.json`,
// or as a directory holding tsconfig.json.
func resolveInPackage(pkg, sub string) (string, bool) {
	var manifest struct {
		Tsconfig string          `json:"tsconfig"`
		Exports  json.RawMessage `json:"exports"`
	}
	if raw, err := os.ReadFile(filepath.Join(pkg, "package.json")); err == nil {
		_ = json.Unmarshal(raw, &manifest)
	}
	var candidates []string
	if sub == "" && manifest.Tsconfig != "" {
		candidates = append(candidates, filepath.Join(pkg, manifest.Tsconfig))
	}
	key := "."
	if sub != "" {
		key = "./" + sub
	}
	for _, k := range []string{key, key + ".json"} {
		if target := exportTarget(manifest.Exports, k); target != "" {
			candidates = append(candidates, filepath.Join(pkg, target))
		}
	}
	if sub == "" {
		candidates = append(candidates, filepath.Join(pkg, "tsconfig.json"))
	} else {
		base := filepath.Join(pkg, filepath.FromSlash(sub))
		candidates = append(candidates, base, base+".json", filepath.Join(base, "tsconfig.json"))
	}
	for _, c := range candidates {
		if isFile(c) {
			return c, true
		}
	}
	return "", false
}

// exportTarget reads one key of a package.json `exports` map: a string, or a
// conditions object whose first known condition is a string.
func exportTarget(exports json.RawMessage, key string) string {
	var table map[string]json.RawMessage
	if len(exports) == 0 || json.Unmarshal(exports, &table) != nil {
		return ""
	}
	entry, ok := table[key]
	if !ok {
		return ""
	}
	var target string
	if json.Unmarshal(entry, &target) == nil {
		return target
	}
	var conditions map[string]json.RawMessage
	if json.Unmarshal(entry, &conditions) != nil {
		return ""
	}
	for _, c := range []string{"default", "require", "import", "types"} {
		if json.Unmarshal(conditions[c], &target) == nil && target != "" {
			return target
		}
	}
	return ""
}

// stripJSONC turns tsconfig's JSON-with-comments into JSON: line and block
// comments outside strings are removed, and a comma before `}` or `]` is
// dropped.
func stripJSONC(src []byte) []byte {
	var out bytes.Buffer
	inString, escaped := false, false
	for i := 0; i < len(src); i++ {
		c := src[i]
		if inString {
			out.WriteByte(c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch {
		case c == '"':
			inString = true
			out.WriteByte(c)
		case c == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
			if i < len(src) {
				out.WriteByte('\n')
			}
		case c == '/' && i+1 < len(src) && src[i+1] == '*':
			end := bytes.Index(src[i+2:], []byte("*/"))
			if end < 0 {
				i = len(src)
			} else {
				i += end + 3
			}
		case c == ',':
			if j := nextSignificant(src, i+1); j < len(src) && (src[j] == '}' || src[j] == ']') {
				continue
			}
			out.WriteByte(c)
		default:
			out.WriteByte(c)
		}
	}
	return out.Bytes()
}

// nextSignificant is the index of the first byte at or after i that is
// neither whitespace nor inside a comment.
func nextSignificant(src []byte, i int) int {
	for i < len(src) {
		switch {
		case src[i] == ' ' || src[i] == '\t' || src[i] == '\n' || src[i] == '\r':
			i++
		case src[i] == '/' && i+1 < len(src) && src[i+1] == '/':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case src[i] == '/' && i+1 < len(src) && src[i+1] == '*':
			end := bytes.Index(src[i+2:], []byte("*/"))
			if end < 0 {
				return len(src)
			}
			i += end + 4
		default:
			return i
		}
	}
	return i
}

// checkoutRoot is the nearest ancestor of dir (dir included) holding a .git
// entry - a worktree's .git file counts - or "" outside any checkout.
func checkoutRoot(dir string) string {
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			return d
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

func isFile(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.Mode().IsRegular()
}

func isDir(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// errNotFound marks a compiler that is not installed where the program is.
var errNotFound = errors.New("not installed")

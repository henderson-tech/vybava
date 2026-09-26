package tsgate

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// Compiler is one resolved TypeScript compiler.
type Compiler struct {
	Dependency string   `json:"dependency"` // the name it is installed under (typescript7)
	Package    string   `json:"package"`    // the package's own name (typescript)
	Version    string   `json:"version"`
	Dir        string   `json:"dir"`     // the package directory, symlinks resolved
	Command    []string `json:"command"` // the argv prefix that runs it
}

// DefaultTS7Dependencies are tried in order when no compiler is configured:
// the pilot's alias, the TS 7 announcement's alias, and `typescript` itself
// once nothing in a repo needs the classic API any more.
var DefaultTS7Dependencies = []string{"typescript7", "@typescript/native", "typescript"}

// baselineDependencies hold the classic (JavaScript) compiler parity compares
// against: `typescript` while it is 6 or 5.9, else the official TS 6 package.
var baselineDependencies = []string{"typescript", "@typescript/typescript6"}

type manifest struct {
	Name    string          `json:"name"`
	Version string          `json:"version"`
	Bin     json.RawMessage `json:"bin"`
}

// findPackage walks up from dir to the first node_modules holding dep.
func findPackage(dir, dep string) (string, manifest, bool) {
	for d := filepath.Clean(dir); ; d = filepath.Dir(d) {
		pkg := filepath.Join(d, "node_modules", filepath.FromSlash(dep))
		if raw, err := os.ReadFile(filepath.Join(pkg, "package.json")); err == nil {
			var m manifest
			if json.Unmarshal(raw, &m) == nil {
				return pkg, m, true
			}
		}
		if filepath.Dir(d) == d {
			return "", manifest{}, false
		}
	}
}

func major(version string) int {
	head, _, _ := strings.Cut(version, ".")
	n, err := strconv.Atoi(head)
	if err != nil {
		return -1
	}
	return n
}

// FindTS7 resolves the TS 7 compiler installed for a program in dir: the
// first of deps whose package is version 7, as its native binary. The npm
// package's `bin/tsc` is only a shim that locates that binary and execs it;
// running it directly needs no node or bun, and works under every linker (a
// by-path `node node_modules/typescript7/bin/tsc` does not under bun's hoisted
// one).
func FindTS7(dir string, deps []string) (Compiler, error) {
	if len(deps) == 0 {
		deps = DefaultTS7Dependencies
	}
	var seen []string
	for _, dep := range deps {
		pkg, m, ok := findPackage(dir, dep)
		if !ok {
			continue
		}
		if major(m.Version) != 7 {
			seen = append(seen, dep+" "+m.Version)
			continue
		}
		real, err := filepath.EvalSymlinks(pkg)
		if err != nil {
			return Compiler{}, err
		}
		exe, err := nativeExe(real, m.Name)
		if err != nil {
			return Compiler{}, fmt.Errorf("%s %s at %s: %w", dep, m.Version, pkg, err)
		}
		return Compiler{Dependency: dep, Package: m.Name, Version: m.Version, Dir: real, Command: []string{exe}}, nil
	}
	found := "nothing"
	if len(seen) > 0 {
		found = strings.Join(seen, ", ")
	}
	return Compiler{}, fmt.Errorf("%w: no TypeScript 7 for %s (tried %s; found %s). Add the alias: \"typescript7\": \"npm:typescript@7.0.2\" in devDependencies",
		errNotFound, dir, strings.Join(deps, ", "), found)
}

// nativeExe finds the platform package the TS 7 shim would exec:
// @typescript/<base>-<platform>-<arch>/lib/<tsc|tsgo>, beside the package
// (bun's isolated store, a global store) or in any node_modules above it.
func nativeExe(pkgDir, name string) (string, error) {
	base := name
	if i := strings.LastIndex(name, "/"); i >= 0 {
		base = name[i+1:]
	}
	bin := "tsgo"
	if base == "typescript" {
		bin = "tsc"
	}
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	platform := "@typescript/" + base + "-" + nodePlatform() + "-" + nodeArch()
	for d := filepath.Dir(pkgDir); ; d = filepath.Dir(d) {
		root := filepath.Join(d, "node_modules")
		if filepath.Base(d) == "node_modules" {
			root = d
		}
		exe := filepath.Join(root, filepath.FromSlash(platform), "lib", bin)
		if isFile(exe) {
			return exe, nil
		}
		if filepath.Dir(d) == d {
			return "", fmt.Errorf("its platform package %s is not installed (optional dependencies skipped?)", platform)
		}
	}
}

// nodePlatform and nodeArch are Node's process.platform and process.arch,
// which name the TS 7 platform packages.
func nodePlatform() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	case "solaris", "illumos":
		return "sunos"
	}
	return runtime.GOOS
}

func nodeArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "386":
		return "ia32"
	case "mips64le":
		return "mips64el"
	case "ppc64le":
		return "ppc64"
	}
	return runtime.GOARCH
}

// FindBaseline resolves the classic compiler a repo runs today (6 or 5.9),
// launched with node, or bun where node is missing.
func FindBaseline(dir string) (Compiler, error) {
	for _, dep := range baselineDependencies {
		pkg, m, ok := findPackage(dir, dep)
		if !ok || major(m.Version) < 4 || major(m.Version) >= 7 {
			continue
		}
		bin := binEntry(m.Bin, "tsc", "tsc6")
		if bin == "" {
			continue
		}
		launcher, err := exec.LookPath("node")
		if err != nil {
			if launcher, err = exec.LookPath("bun"); err != nil {
				return Compiler{}, fmt.Errorf("%s %s needs node or bun on PATH", dep, m.Version)
			}
		}
		real, err := filepath.EvalSymlinks(pkg)
		if err != nil {
			return Compiler{}, err
		}
		return Compiler{Dependency: dep, Package: m.Name, Version: m.Version, Dir: real, Command: []string{launcher, filepath.Join(real, bin)}}, nil
	}
	return Compiler{}, fmt.Errorf("%w: no classic TypeScript (6 or 5.9) for %s to compare against", errNotFound, dir)
}

// binEntry reads a package.json `bin`: the first named entry present.
func binEntry(raw json.RawMessage, names ...string) string {
	var table map[string]string
	if json.Unmarshal(raw, &table) != nil {
		return ""
	}
	for _, n := range names {
		if p := table[n]; p != "" {
			return filepath.FromSlash(p)
		}
	}
	return ""
}

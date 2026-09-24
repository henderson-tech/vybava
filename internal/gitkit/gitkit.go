// Package gitkit is the deterministic layer the git-family skills (prm,
// push-all, sync) execute: PR selector parsing, review-thread triage, merge
// preconditions, worktree resolution, path classification, DB-url safety.
//
// The scripts are zero-dependency, erasable TypeScript embedded in the
// binary and run by Node's native type stripping. Skills call the stable
// surface `vybava gitkit <script> [args]`, never a file path, so a script
// can be ported to Go behind the same verb without touching any skill.
package gitkit

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

//go:embed ts/bin/*.ts
var payload embed.FS

// Closed diagnostic codes.
const (
	DiagUnknownScript = "GITKIT_UNKNOWN_SCRIPT"
	DiagNodeMissing   = "GITKIT_NODE_MISSING"
	DiagNodeTooOld    = "GITKIT_NODE_TOO_OLD"
)

// MinNode names the oldest Node that strips types without a flag.
const MinNode = "22.18.0 (or 23.6.0+)"

// Scripts lists the runnable script names (file stems), sorted.
func Scripts() []string {
	entries, _ := fs.ReadDir(payload, "ts/bin")
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, strings.TrimSuffix(entry.Name(), ".ts"))
	}
	sort.Strings(names)
	return names
}

// Digest is the content address of the embedded payload.
func Digest() string {
	hash := sha256.New()
	for _, name := range Scripts() {
		data, _ := payload.ReadFile("ts/bin/" + name + ".ts")
		fmt.Fprintf(hash, "%s\x00%d\x00", name, len(data))
		hash.Write(data)
	}
	return hex.EncodeToString(hash.Sum(nil))[:16]
}

// Materialize writes the payload under cacheRoot/<digest>/bin once and
// returns that bin directory. Scripts import each other by relative path,
// so the tree is written whole and activated with one rename.
func Materialize(cacheRoot string) (string, error) {
	final := filepath.Join(cacheRoot, Digest())
	bin := filepath.Join(final, "bin")
	if _, err := os.Stat(bin); err == nil {
		return bin, nil
	}
	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		return "", fmt.Errorf("create gitkit cache: %w", err)
	}
	staging, err := os.MkdirTemp(cacheRoot, ".staging-*")
	if err != nil {
		return "", fmt.Errorf("create gitkit staging: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()
	if err := os.MkdirAll(filepath.Join(staging, "bin"), 0o755); err != nil {
		return "", err
	}
	for _, name := range Scripts() {
		data, err := payload.ReadFile(path.Join("ts/bin", name+".ts"))
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(staging, "bin", name+".ts"), data, 0o644); err != nil {
			return "", fmt.Errorf("stage %s: %w", name, err)
		}
	}
	if err := os.Rename(staging, final); err != nil {
		// A concurrent invocation activated the same digest first.
		if _, statErr := os.Stat(bin); statErr == nil {
			return bin, nil
		}
		return "", fmt.Errorf("activate gitkit payload: %w", err)
	}
	return bin, nil
}

// DefaultCacheRoot is <user cache>/vybava/gitkit.
func DefaultCacheRoot() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "vybava", "gitkit"), nil
}

// Node is a resolved Node runtime.
type Node struct {
	Path    string `json:"path"`
	Version string `json:"version"`
}

// NodeError carries a closed diagnostic code and its fix.
type NodeError struct {
	Code   string
	Detail string
	Fix    string
}

func (e NodeError) Error() string { return e.Code + ": " + e.Detail }

var versionPattern = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)`)

// StripsTypes reports whether a Node version runs .ts without a flag.
func StripsTypes(version string) bool {
	match := versionPattern.FindStringSubmatch(strings.TrimSpace(version))
	if match == nil {
		return false
	}
	major, _ := strconv.Atoi(match[1])
	minor, _ := strconv.Atoi(match[2])
	switch {
	case major >= 24:
		return true
	case major == 23:
		return minor >= 6
	case major == 22:
		return minor >= 18
	}
	return false
}

// ResolveNode finds node on PATH and checks it can run the payload.
func ResolveNode(lookPath func(string) (string, error), version func(string) (string, error)) (Node, error) {
	nodePath, err := lookPath("node")
	if err != nil {
		return Node{}, NodeError{Code: DiagNodeMissing, Detail: "node is not on PATH; gitkit scripts need Node " + MinNode, Fix: "brew install node"}
	}
	v, err := version(nodePath)
	if err != nil {
		return Node{}, fmt.Errorf("read node version: %w", err)
	}
	node := Node{Path: nodePath, Version: strings.TrimSpace(v)}
	if !StripsTypes(node.Version) {
		return node, NodeError{Code: DiagNodeTooOld, Detail: fmt.Sprintf("node %s at %s cannot run TypeScript natively; need %s", node.Version, nodePath, MinNode), Fix: "brew upgrade node"}
	}
	return node, nil
}

// NodeVersion runs `node --version`.
func NodeVersion(nodePath string) (string, error) {
	out, err := exec.Command(nodePath, "--version").Output()
	return string(out), err
}

// Argv is the node invocation for a script: argv[0] first, ready for exec.
func Argv(node Node, bin, script string, args []string) []string {
	argv := []string{node.Path, "--disable-warning=ExperimentalWarning", filepath.Join(bin, script+".ts")}
	return append(argv, args...)
}

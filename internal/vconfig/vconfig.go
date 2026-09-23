// Package vconfig loads the shared per-repository Výbava configuration:
// vybava.config.ts (evaluated by bun, the repo's TypeScript is the source of
// truth) or vybava.config.json (the same shape, for machines without bun).
// Applets read their own section and never parse the file themselves.
//
// The evaluated document is cached next to the repo's git metadata, keyed by
// the config file's size+mtime, so hooks and repeated CLI calls pay the bun
// evaluation (~0.5 s cold) once per edit, not once per call.
package vconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// FileTS and FileJSON are the two accepted config files, in precedence order.
const (
	FileTS   = "vybava.config.ts"
	FileJSON = "vybava.config.json"
	// HelperDir holds the generated TypeScript helpers the config imports.
	HelperDir = ".vybava"
	// HelperFile is the generated helper module (defineConfig, englishAsKey, …).
	HelperFile = "config.ts"
)

// ErrNotFound means no config file exists in cwd or any parent.
var ErrNotFound = errors.New("no vybava.config.ts or vybava.config.json found in this directory or any parent")

// Config is one evaluated configuration document.
type Config struct {
	Root     string                     // directory holding the config file
	Path     string                     // the config file itself
	Sections map[string]json.RawMessage // top-level keys: lok, hotfix, …
}

// Section decodes one top-level key into v. A missing section is ErrNoSection.
func (c *Config) Section(name string, v any) error {
	raw, ok := c.Sections[name]
	if !ok {
		return fmt.Errorf("%w: %q", ErrNoSection, name)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("section %q: %w", name, err)
	}
	return nil
}

// ErrNoSection means the config has no entry for the requested applet.
var ErrNoSection = errors.New("config has no section")

// Find walks up from dir to the first directory holding a config file.
func Find(dir string) (root, path string, err error) {
	dir, err = filepath.Abs(dir)
	if err != nil {
		return "", "", err
	}
	for {
		for _, name := range []string{FileTS, FileJSON} {
			p := filepath.Join(dir, name)
			if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
				return dir, p, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", "", ErrNotFound
		}
		dir = parent
	}
}

// Load finds and evaluates the config for dir, using the cache when fresh.
func Load(dir string) (*Config, error) {
	root, path, err := Find(dir)
	if err != nil {
		return nil, err
	}
	raw, err := evaluate(root, path)
	if err != nil {
		return nil, err
	}
	sections := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &sections); err != nil {
		return nil, fmt.Errorf("%s: evaluated config is not a JSON object: %w", path, err)
	}
	return &Config{Root: root, Path: path, Sections: sections}, nil
}

type cacheEntry struct {
	Size    int64           `json:"size"`
	ModTime int64           `json:"mtime"`
	Doc     json.RawMessage `json:"doc"`
}

func cachePath(root, path string) string {
	// Prefer the git dir (per worktree, never committed); fall back to the
	// user cache dir keyed by the config's absolute path.
	if dir := gitDir(root); dir != "" {
		return filepath.Join(dir, "vybava-config-cache.json")
	}
	base, err := os.UserCacheDir()
	if err != nil {
		base = os.TempDir()
	}
	return filepath.Join(base, "vybava", "config-cache", strings.NewReplacer("/", "_", ":", "_").Replace(path)+".json")
}

// gitDir finds the git dir of the repository holding dir without forking
// git, which every hook call would otherwise pay: a `.git` directory, or the
// `gitdir:` a linked worktree's or submodule's `.git` file points to. "" when
// dir is in no repository.
func gitDir(dir string) string {
	for {
		p := filepath.Join(dir, ".git")
		if st, err := os.Stat(p); err == nil {
			if st.IsDir() {
				return p
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return ""
			}
			target, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
			if !ok {
				return ""
			}
			if target = strings.TrimSpace(target); !filepath.IsAbs(target) {
				target = filepath.Join(dir, target)
			}
			if st, err := os.Stat(target); err != nil || !st.IsDir() {
				return "" // a pruned worktree: never create its git dir for a cache
			}
			return target
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func evaluate(root, path string) (json.RawMessage, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	cp := cachePath(root, path)
	if raw, err := os.ReadFile(cp); err == nil {
		var e cacheEntry
		if json.Unmarshal(raw, &e) == nil && e.Size == st.Size() && e.ModTime == st.ModTime().UnixNano() {
			return e.Doc, nil
		}
	}
	var doc json.RawMessage
	if strings.HasSuffix(path, ".json") {
		doc, err = os.ReadFile(path)
	} else {
		doc, err = evaluateTS(path)
	}
	if err != nil {
		return nil, err
	}
	if !json.Valid(doc) {
		return nil, fmt.Errorf("%s: evaluation did not produce JSON", path)
	}
	_ = os.MkdirAll(filepath.Dir(cp), 0o755)
	if raw, err := json.Marshal(cacheEntry{Size: st.Size(), ModTime: st.ModTime().UnixNano(), Doc: doc}); err == nil {
		_ = os.WriteFile(cp, raw, 0o644)
	}
	return doc, nil
}

// ErrBunMissing means the TypeScript config cannot be evaluated on this machine.
var ErrBunMissing = errors.New("bun is not on PATH — vybava.config.ts needs bun to evaluate; install bun or ship vybava.config.json instead")

func evaluateTS(path string) (json.RawMessage, error) {
	bun, err := exec.LookPath("bun")
	if err != nil {
		return nil, ErrBunMissing
	}
	script := fmt.Sprintf(`const m = await import(%q); const c = m.default ?? m; process.stdout.write(JSON.stringify(c));`, path)
	cmd := exec.Command(bun, "-e", script)
	cmd.Dir = filepath.Dir(path)
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	done := make(chan error, 1)
	go func() { done <- cmd.Run() }()
	select {
	case err := <-done:
		if err != nil {
			return nil, fmt.Errorf("%s: bun evaluation failed: %s", path, strings.TrimSpace(stderr.String()))
		}
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("%s: bun evaluation timed out after 20s", path)
	}
	return json.RawMessage(bytes.TrimSpace(out.Bytes())), nil
}

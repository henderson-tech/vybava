// Package toolsetup installs and updates the catalog's `tool` items — the
// external apps and CLIs a henderson-tech Mac runs on — through each
// product's own published channel: Homebrew, the apps.fixit.app pultik
// shelf, bun, or the product's installer. A tool is detected live by its
// probe and never recorded in Výbava state, so a tool installed by hand
// counts as installed.
package toolsetup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/henderson-tech/vybava/internal/catalog"
)

// Closed diagnostic codes.
const (
	DiagNeedsMissing   = "TOOL_NEEDS_MISSING"
	DiagInstallFailed  = "TOOL_INSTALL_FAILED"
	DiagShelfMissing   = "TOOL_SHELF_APP_MISSING"
	DiagChecksum       = "TOOL_CHECKSUM_MISMATCH"
	DiagNeedsHuman     = "TOOL_NEEDS_HUMAN"
	DiagProbeAfterFail = "TOOL_PROBE_FAILED_AFTER_INSTALL"
)

// Env is everything the recipes touch, injectable for tests.
type Env struct {
	Home     string
	Arch     string   // runtime.GOARCH
	AppDirs  []string // where .app bundles live, first is the install target
	LookPath func(string) (string, error)
	// Exec runs argv; interactive attaches the terminal.
	Exec     func(argv []string, interactive bool) error
	Output   func(argv []string) (string, error)
	Shelf    Shelf
	BinDir   string // raw CLI binaries land here
	TempRoot string
}

// DefaultEnv is the real machine.
func DefaultEnv(home string, shelf Shelf, exec func([]string, bool) error, output func([]string) (string, error), lookPath func(string) (string, error)) Env {
	return Env{
		Home: home, Arch: runtime.GOARCH,
		AppDirs:  []string{"/Applications", filepath.Join(home, "Applications")},
		LookPath: lookPath, Exec: exec, Output: output, Shelf: shelf,
		BinDir: filepath.Join(home, ".local", "bin"),
	}
}

// Diag is a failed step with its closed code and exact next command.
type Diag struct {
	Code   string
	Detail string
	Fix    string
}

func (d Diag) Error() string { return d.Code + ": " + d.Detail }

// Result is one tool's outcome.
type Result struct {
	ID      string `json:"id"`
	Action  string `json:"action"` // present | current | installed | updated | would-install | would-update | skipped | failed
	Channel string `json:"channel"`
	Detail  string `json:"detail,omitempty"`
	// Setup lists the guided commands still owed, as command lines.
	Setup []string `json:"setup,omitempty"`
	// SetupArgv is Setup ready to run: a command named like the probed
	// binary runs by its probed path, since ~/.local/bin may not be on PATH yet.
	SetupArgv [][]string `json:"-"`
}

// Probe reports whether the tool is installed and where it was seen.
func Probe(env Env, tool catalog.Tool) (bool, string) {
	switch {
	case tool.Probe.App != "":
		for _, dir := range env.AppDirs {
			path := filepath.Join(dir, tool.Probe.App)
			if isDir(path) {
				return true, path
			}
		}
	case tool.Probe.Command != "":
		if path, err := env.LookPath(tool.Probe.Command); err == nil {
			return true, path
		}
		path := filepath.Join(env.BinDir, tool.Probe.Command)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return true, path
		}
	case tool.Probe.Path != "":
		path := expandHome(tool.Probe.Path, env.Home)
		if _, err := os.Stat(path); err == nil {
			return true, path
		}
	}
	return false, ""
}

// Order sorts tool items so every item follows its needs; items outside
// the selection are ignored (a missing need is caught at apply time).
func Order(items []catalog.Item) ([]catalog.Item, error) {
	byID := make(map[string]catalog.Item, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}
	const (
		visiting = 1
		done     = 2
	)
	state := make(map[string]int, len(items))
	ordered := make([]catalog.Item, 0, len(items))
	var visit func(id string) error
	visit = func(id string) error {
		item, selected := byID[id]
		if !selected {
			return nil
		}
		switch state[id] {
		case done:
			return nil
		case visiting:
			return fmt.Errorf("tool %q is part of a needs cycle", id)
		}
		state[id] = visiting
		for _, need := range item.Tool.Needs {
			if err := visit(need); err != nil {
				return err
			}
		}
		state[id] = done
		ordered = append(ordered, item)
		return nil
	}
	for _, item := range items {
		if err := visit(item.ID); err != nil {
			return nil, err
		}
	}
	return ordered, nil
}

// Options steer one Apply.
type Options struct {
	Update      bool // move present tools to their channel's latest
	DryRun      bool
	Interactive bool // a human is at the terminal
}

// Apply installs (or updates) one tool; ApplyAll owns the needs check.
func Apply(env Env, item catalog.Item, opts Options) (Result, error) {
	tool := *item.Tool
	channel, target := tool.Install.Channel()
	res := Result{ID: item.ID, Channel: channel}
	present, where := Probe(env, tool)
	switch {
	case present && !opts.Update:
		res.Action, res.Detail = "present", where
		return res, nil
	case tool.Interactive && !opts.Interactive:
		res.Action = "skipped"
		return res, Diag{Code: DiagNeedsHuman, Detail: item.ID + " installs through a guided flow that needs a terminal", Fix: "vybava setup team"}
	case opts.DryRun && present:
		res.Action, res.Detail = "would-update", target
		return res, nil
	case opts.DryRun:
		res.Action, res.Detail = "would-install", target
		return res, nil
	}

	var err error
	switch channel {
	case "brew_cask":
		err = brew(env, "--cask", target, present)
	case "brew":
		err = brew(env, "", target, present)
	case "bun":
		pkg := target
		if present {
			pkg += "@latest"
		}
		err = env.Exec([]string{"bun", "add", "-g", pkg}, false)
	case "pultik":
		res.Detail, err = pultik(env, strings.ReplaceAll(target, "{arch}", env.Arch), tool, present)
	case "run":
		err = env.Exec(tool.Install.Run, tool.Interactive)
	}
	if errors.Is(err, errCurrent) {
		res.Action = "current"
		return res, nil
	}
	if err != nil {
		res.Action = "failed"
		var diag Diag
		if errors.As(err, &diag) {
			return res, diag
		}
		return res, Diag{Code: DiagInstallFailed, Detail: fmt.Sprintf("%s via %s: %v", item.ID, channel, err), Fix: "vybava setup team --only " + item.ID}
	}
	if ok, _ := Probe(env, tool); !ok {
		res.Action = "failed"
		return res, Diag{Code: DiagProbeAfterFail, Detail: item.ID + " installed but its probe still fails — the recipe's probe or channel is wrong", Fix: "vybava catalog list --json"}
	}
	if present {
		res.Action = "updated"
		return res, nil
	}
	res.Action = "installed"
	_, where = Probe(env, tool)
	for _, argv := range tool.Setup {
		resolved := append([]string{}, argv...)
		if tool.Probe.Command != "" && resolved[0] == tool.Probe.Command {
			resolved[0] = where
		}
		res.Setup = append(res.Setup, strings.Join(resolved, " "))
		res.SetupArgv = append(res.SetupArgv, resolved)
	}
	return res, nil
}

func brew(env Env, cask, target string, present bool) error {
	verb := "install"
	if present {
		verb = "upgrade"
	}
	argv := []string{"brew", verb}
	if cask != "" {
		argv = append(argv, cask)
	}
	return env.Exec(append(argv, target), false)
}

func expandHome(path, home string) string {
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// Outcome pairs a tool's result with the diagnostic that stopped it.
type Outcome struct {
	Result Result
	Diag   *Diag
}

// ApplyAll runs the selected tools in needs order. catalogTools holds every
// tool item so a need left out of the selection is still probed: installed
// counts, missing stops the dependent tool with the command that fixes it.
func ApplyAll(env Env, selected []catalog.Item, catalogTools map[string]catalog.Item, opts Options) ([]Outcome, error) {
	ordered, err := Order(selected)
	if err != nil {
		return nil, err
	}
	failed := map[string]bool{}
	outcomes := make([]Outcome, 0, len(ordered))
	for _, item := range ordered {
		if diag := missingNeed(env, item, catalogTools, failed, opts.DryRun); diag != nil {
			failed[item.ID] = true
			channel, _ := item.Tool.Install.Channel()
			outcomes = append(outcomes, Outcome{Result: Result{ID: item.ID, Action: "skipped", Channel: channel}, Diag: diag})
			continue
		}
		res, err := Apply(env, item, opts)
		outcome := Outcome{Result: res}
		if err != nil {
			var diag Diag
			if !errors.As(err, &diag) {
				diag = Diag{Code: DiagInstallFailed, Detail: err.Error()}
			}
			outcome.Diag = &diag
			failed[item.ID] = true
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes, nil
}

func missingNeed(env Env, item catalog.Item, catalogTools map[string]catalog.Item, failed map[string]bool, dryRun bool) *Diag {
	for _, need := range item.Tool.Needs {
		if failed[need] {
			return &Diag{Code: DiagNeedsMissing, Detail: item.ID + " needs " + need + ", which did not install", Fix: "vybava setup team --only " + need}
		}
		if dryRun {
			continue // a need planned in this same run would be installed first
		}
		if ok, _ := Probe(env, *catalogTools[need].Tool); !ok {
			return &Diag{Code: DiagNeedsMissing, Detail: item.ID + " needs " + need + ", which is not installed", Fix: "vybava setup team --only " + need}
		}
	}
	return nil
}

// Selection picks what a run applies from a group's items: `only` wins
// outright; otherwise every non-optional item, plus the optional ones named
// in `with`. Unknown names are returned so the caller can refuse them.
func Selection(items []catalog.Item, only, with []string) (selected []catalog.Item, unknown []string) {
	byID := make(map[string]catalog.Item, len(items))
	for _, item := range items {
		byID[item.ID] = item
	}
	pick := map[string]bool{}
	for _, id := range append(append([]string{}, only...), with...) {
		if _, ok := byID[id]; !ok {
			unknown = append(unknown, id)
		}
	}
	for _, id := range only {
		pick[id] = true
	}
	if len(only) == 0 {
		for _, item := range items {
			pick[item.ID] = item.Tool == nil || !item.Tool.Optional
		}
		for _, id := range with {
			pick[id] = true
		}
	}
	for _, item := range items {
		if pick[item.ID] {
			selected = append(selected, item)
		}
	}
	return selected, unknown
}

// DiagBinNotOnPath: the CLIs landed in BinDir but a new shell will not find them.
const DiagBinNotOnPath = "SETUP_BIN_NOT_ON_PATH"

// PathGap reports BinDir missing from a PATH value, entries compared cleaned,
// with a fix that names BinDir itself ($HOME-relative when it lives there).
func PathGap(env Env, pathValue string) *Diag {
	want := filepath.Clean(env.BinDir)
	for _, entry := range filepath.SplitList(pathValue) {
		if entry != "" && filepath.Clean(entry) == want {
			return nil
		}
	}
	dir := want
	if rel, err := filepath.Rel(env.Home, want); err == nil && !strings.HasPrefix(rel, "..") {
		dir = "$HOME/" + rel
	}
	return &Diag{
		Code:   DiagBinNotOnPath,
		Detail: want + " holds the installed CLIs but is not on PATH",
		Fix:    `echo 'export PATH="` + dir + `:$PATH"' >> ~/.zprofile`,
	}
}

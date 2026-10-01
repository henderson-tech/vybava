// Package uiloop is the `ui-loop` applet: the deterministic layer of the UI
// polish loop (map → capture + lint → review → fix → verify). It embeds the
// TypeScript/Playwright harness (harness/) and syncs it into each repo's
// <dir>/vendor with a stamp; `check` gates drift, `run` writes the pass's
// run.json and runs (or prints) the repo's own Playwright against it, and
// `split`, `publish` and `scoreboard` turn a pass directory into vitrinka sets
// and a scoreboard; `state`, `batches`, `merge-review` and `lanes` (stage.go,
// lanes.go) own every list the review-loop stages hand on. The orchestration
// (reviewers, fix lanes) lives in the vitrinka map / review-loop workflows,
// which drive this CLI.
package uiloop

import (
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/henderson-tech/vybava/internal/vconfig"
)

// Section is the vybava.config.ts key this applet owns.
const Section = "uiLoop"

// Tool is one repository's ui-loop configuration plus the embedded harness.
type Tool struct {
	Root       string // directory holding vybava.config.ts
	ConfigPath string
	// Raw is the section as configured; Config has the defaults filled.
	Raw     Config
	Config  Config
	Version string
	Harness fs.FS
	// Log receives the output of the commands a verb runs (never stdout,
	// which carries the envelope).
	Log  io.Writer
	Exec ExecFunc
	Now  func() time.Time
	// LookPath and Sleep are swapped out by tests.
	LookPath func(string) (string, error)
	Sleep    func(time.Duration)
}

// Result is what a verb hands the envelope.
type Result struct {
	Data        any
	Diagnostics []runx.Diagnostic
	Next        []string
}

// Cmd is one external command a verb runs.
type Cmd struct {
	Dir  string
	Env  []string // added to the process environment
	Args []string
	// Stream, when set, receives stdout and stderr live instead of capturing them.
	Stream  io.Writer
	Timeout time.Duration
}

// CmdOut is a finished command.
type CmdOut struct {
	Code   int
	Stdout string
	Stderr string
}

// ExecFunc runs a command; a non-zero exit is CmdOut.Code, never an error.
type ExecFunc func(ctx context.Context, c Cmd) (CmdOut, error)

// RealExec runs commands on this machine.
func RealExec(ctx context.Context, c Cmd) (CmdOut, error) {
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, c.Args[0], c.Args[1:]...)
	cmd.Dir = c.Dir
	cmd.Env = append(os.Environ(), c.Env...)
	var stdout, stderr bytes.Buffer
	if c.Stream != nil {
		cmd.Stdout, cmd.Stderr = c.Stream, c.Stream
	} else {
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
	}
	err := cmd.Run()
	out := CmdOut{Stdout: stdout.String(), Stderr: stderr.String()}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		out.Code = exitErr.ExitCode()
		if ctx.Err() != nil {
			out.Stderr += "\n" + ctx.Err().Error()
		}
		return out, nil
	}
	return out, err
}

// Open loads and validates the section for cwd.
func Open(cwd, version string, log io.Writer) (*Tool, error) {
	cfg, err := vconfig.Load(cwd)
	if err != nil {
		if errors.Is(err, vconfig.ErrNotFound) {
			return nil, diag(DiagConfigMissing, err.Error(), "add a uiLoop section to vybava.config.ts (docs/uiloop.md)")
		}
		return nil, diag(DiagConfigInvalid, err.Error(), "fix the config file so `vybava config show --json` evaluates it")
	}
	var raw Config
	if err := cfg.Section(Section, &raw); err != nil {
		if errors.Is(err, vconfig.ErrNoSection) {
			return nil, diag(DiagConfigMissing, cfg.Path+" has no uiLoop section", "add one (docs/uiloop.md has the shape)")
		}
		return nil, diag(DiagConfigInvalid, err.Error(), "fix the uiLoop section in "+cfg.Path+" (unknown keys are rejected; docs/uiloop.md has the shape)")
	}
	return New(cfg.Root, cfg.Path, raw, version, log)
}

// New builds a Tool from an already decoded section (tests use it directly).
func New(root, configPath string, raw Config, version string, log io.Writer) (*Tool, error) {
	if problems := raw.Validate(); len(problems) > 0 {
		return nil, diag(DiagConfigInvalid, strings.Join(problems, "; "), "fix the uiLoop section in "+configPath)
	}
	harness, err := fs.Sub(harnessFS, "harness")
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = io.Discard
	}
	return &Tool{
		Root: root, ConfigPath: configPath, Raw: raw, Config: raw.WithDefaults(),
		Version: version, Harness: harness, Log: log, Exec: RealExec, Now: time.Now,
		LookPath: exec.LookPath, Sleep: time.Sleep,
	}, nil
}

// abs resolves a repo-relative path.
func (t *Tool) abs(rel string) string { return filepath.Join(t.Root, filepath.FromSlash(rel)) }

// VendorDir is <dir>/vendor.
func (t *Tool) VendorDir() string { return t.abs(t.Config.Dir + "/vendor") }

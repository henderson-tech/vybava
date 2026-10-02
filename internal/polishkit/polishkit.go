// Package polishkit is the `polish-kit` applet: the deterministic layer of
// the polish skill (target app|ui|api x intensity quick|default|full). The
// skill stays thin; this package owns the mechanical parts: target inference
// from the diff, device lanes (simulators, phones, emulators, URLs), the pass
// ledger (run.json with its cell table), native screenshot capture, contact
// sheets and the report. It never touches the machine's network (a browser
// or server lane is one GET of the configured URL) and never modifies app
// code. Every external command goes through Tool.Exec so tests never reach
// a device.
package polishkit

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/henderson-tech/vybava/internal/vconfig"
)

// Section is the vybava.config.ts key this applet owns.
const Section = "polish"

// Tool is one repository's polish configuration plus the seams a verb
// reaches the machine through.
type Tool struct {
	Root       string // directory holding vybava.config.ts (the repo root)
	ConfigPath string
	// Cwd is where the verb was invoked; plan widens targets by it.
	Cwd string
	// Raw is the section as configured; Config has the defaults filled.
	Raw     Config
	Config  Config
	Version string
	// Log receives progress lines (never stdout, which carries the envelope).
	Log io.Writer
	// Exec runs an external command; LookPath finds one; HTTPGet probes a
	// URL lane; Now and Sleep are swapped out by tests.
	Exec     ExecFunc
	LookPath func(string) (string, error)
	HTTPGet  func(url string, timeout time.Duration) (status int, err error)
	Now      func() time.Time
	Sleep    func(time.Duration)
	// TempDir is where devicectl writes its JSON (a file is its only output).
	TempDir func() string
}

// Result is what a verb hands the envelope: Data for --json, Lines for a
// human (the same facts as a short table), diagnostics and next commands.
type Result struct {
	Data        any
	Lines       []string
	Diagnostics []runx.Diagnostic
	Next        []string
}

// Cmd is one external command a verb runs.
type Cmd struct {
	Dir     string
	Args    []string
	Timeout time.Duration
}

// CmdOut is a finished command; Stdout may carry binary (a screencap).
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
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
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

// RealHTTPGet is the one network call the applet makes: a GET of a
// configured lane URL, bounded by timeout.
func RealHTTPGet(url string, timeout time.Duration) (int, error) {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, nil
}

// Open loads and validates the section for cwd.
func Open(cwd, version string, log io.Writer) (*Tool, error) {
	cfg, err := vconfig.Load(cwd)
	if err != nil {
		if errors.Is(err, vconfig.ErrNotFound) {
			return nil, diag(DiagNoConfigSection, err.Error(), configFix)
		}
		return nil, diag(DiagNoConfigSection, err.Error(), "fix the config file so `vybava config show --json` evaluates it, then "+configFix)
	}
	var raw Config
	if err := cfg.Section(Section, &raw); err != nil {
		if errors.Is(err, vconfig.ErrNoSection) {
			return nil, diag(DiagNoConfigSection, cfg.Path+" has no polish section", configFix)
		}
		return nil, diag(DiagNoConfigSection, err.Error(), "fix the polish section in "+cfg.Path+" (unknown keys are rejected); "+configFix)
	}
	return New(cfg.Root, cfg.Path, cwd, raw, version, log)
}

// New builds a Tool from an already decoded section (tests use it directly).
func New(root, configPath, cwd string, raw Config, version string, log io.Writer) (*Tool, error) {
	if problems := raw.Validate(); len(problems) > 0 {
		return nil, diag(DiagNoConfigSection, "polish section invalid: "+strings.Join(problems, "; "), "fix the polish section in "+configPath+"; "+configFix)
	}
	if log == nil {
		log = io.Discard
	}
	return &Tool{
		Root: root, ConfigPath: configPath, Cwd: cwd, Raw: raw, Config: raw.WithDefaults(),
		Version: version, Log: log, Exec: RealExec, LookPath: exec.LookPath, HTTPGet: RealHTTPGet,
		Now: time.Now, Sleep: time.Sleep, TempDir: os.TempDir,
	}, nil
}

// OutDir is the absolute run root.
func (t *Tool) OutDir() string { return filepath.Join(t.Root, filepath.FromSlash(t.Config.Out)) }

// configFix is the exact block a repo without a section adds.
const configFix = "add to vybava.config.ts: polish: { targets: { app: ['apps/client/**'], ui: ['apps/web/**'], api: ['apps/api/**'] }, lanes: [{ id: 'ios26', target: 'app', kind: 'ios-sim', runtime: '26', deviceType: 'iPhone 17 Pro' }], screens: [{ id: 'home', title: 'Home', target: 'app', url: 'myapp://home' }] } (docs/polish-kit.md)"

// run runs a command from the repo root, summarising a failure as an infra
// error that names the command and the tail of its stderr.
func (t *Tool) run(ctx context.Context, timeout time.Duration, args ...string) (CmdOut, error) {
	out, err := t.Exec(ctx, Cmd{Dir: t.Root, Args: args, Timeout: timeout})
	if err != nil {
		return out, &execError{args: args, err: err}
	}
	return out, nil
}

// execError is a command that could not run at all (not found, killed).
type execError struct {
	args []string
	err  error
}

func (e *execError) Error() string {
	return strings.Join(e.args, " ") + ": " + e.err.Error()
}

func (e *execError) Unwrap() error { return e.err }

// stderrTail keeps a failure message short: the last non-empty line.
func stderrTail(out CmdOut) string {
	lines := strings.Split(strings.TrimSpace(out.Stderr+"\n"+out.Stdout), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			return s
		}
	}
	return "exit " + itoa(out.Code)
}

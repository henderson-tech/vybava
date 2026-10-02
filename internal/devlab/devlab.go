// Package devlab is the machine-global ledger of physical test phones and
// the leases that give exactly one holder a phone at a time. perflab drives
// every verb through it; polish-kit and a future journeys device lane import
// it for discovery, so the ledger is not perf-only.
//
// Layout (docs/perflab.md "Device ledger and leases"):
//
//	$PERFLAB_STATE_DIR (default ~/.local/state/perflab)
//	├─ devices.json            the ledger, schemaVersion 1
//	├─ leases/<deviceId>.json  one lease record per device (held or free)
//	└─ locks/                  kernel flocks: ledger, lease-<id>, device-<id>
//
// Three properties are load-bearing:
//
//   - The lease token is the capability. In-process subagents share
//     CLAUDE_PID and often CLAUDE_CODE_SESSION_ID with their parent, and a
//     resumed copy runs beside the original (memo #206), so neither id tells
//     two copies apart. A token minted after the fork does; only its sha256
//     is stored, and it is printed exactly once, by Acquire.
//   - No force-steal. A lease is reclaimed only when it is expired AND its
//     holder process is provably dead (pid gone or its start time changed).
//     An unknown holder (no claude process recorded) counts as alive until
//     the TTL ends, so the TTL always bounds a lease.
//   - Every external command goes through Lab.Exec, so tests run on recorded
//     transcripts and never reach a device. Children run in the invoking
//     process's lifetime; devlab never daemonises anything, and never kills
//     by pattern: it stops only the process groups a lease recorded.
package devlab

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/runx"
)

// Platform is the closed set of device platforms the lab drives.
type Platform string

const (
	PlatformIOS     Platform = "ios"
	PlatformAndroid Platform = "android"
)

// Result is what a verb hands the envelope: Data for --json, Lines for a
// human (the same facts, short), diagnostics and next commands.
type Result struct {
	Data        any
	Lines       []string
	Diagnostics []runx.Diagnostic
	Next        []string
}

// Cmd is one external command. Stdout, when set, receives the command's
// standard output as it streams (a pull or a screencap) instead of
// CmdOut.Stdout.
type Cmd struct {
	Args    []string
	Timeout time.Duration
	Stdout  io.Writer
}

// CmdOut is a finished command; a non-zero exit is Code, never an error.
type CmdOut struct {
	Code   int
	Stdout string
	Stderr string
}

// ExecFunc runs a command. The error is reserved for a command that could
// not run at all (not found, killed by the timeout).
type ExecFunc func(ctx context.Context, c Cmd) (CmdOut, error)

// Lab is the per-invocation handle on the ledger. Every machine touch is a
// field, so tests swap the exec runner, the clock, the environment and the
// process table.
type Lab struct {
	// StateDir holds devices.json, leases/ and locks/.
	StateDir string
	// ProjectDir is the adapter root the lease owner is attributed to
	// (perflab's --project); empty means the current directory.
	ProjectDir string
	Exec       ExecFunc
	LookPath   func(string) (string, error)
	Now        func() time.Time
	Getenv     func(string) string
	// ProcStart reports whether pid is a live process and, when it is,
	// its start time (second precision). ok=false with a nil error means
	// the process is gone; an error means liveness is unknown.
	ProcStart func(pid int) (start time.Time, ok bool, err error)
	// StopGroup sends SIGTERM to a process group a lease recorded.
	StopGroup func(pgid int) error
	// HTTPGet fetches the RemoteXPC tunnel registry.
	HTTPGet func(ctx context.Context, url string) ([]byte, error)
	// TempDir is where devicectl writes its JSON (a file is its only
	// machine-readable output).
	TempDir func() string
	// LockWait bounds every wait for the ledger or a lease file lock.
	LockWait time.Duration
	// Pid is the invoking process (the lease's invokerPid).
	Pid int
}

// DefaultLockWait is the design's lease-verb flock wait (docs/perflab.md
// "Per-verb default timeouts").
const DefaultLockWait = 30 * time.Second

// StateDir is ~/.local/state/perflab, or $PERFLAB_STATE_DIR (tests,
// sandboxes); the blip convention.
func StateDir() (string, error) {
	if dir := os.Getenv("PERFLAB_STATE_DIR"); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "perflab"), nil
}

// Open returns a Lab wired to this machine.
func Open() (*Lab, error) {
	dir, err := StateDir()
	if err != nil {
		return nil, err
	}
	return &Lab{
		StateDir:  dir,
		Exec:      RealExec,
		LookPath:  exec.LookPath,
		Now:       time.Now,
		Getenv:    os.Getenv,
		ProcStart: procStart,
		StopGroup: stopGroup,
		HTTPGet:   httpGet,
		TempDir:   os.TempDir,
		LockWait:  DefaultLockWait,
		Pid:       os.Getpid(),
	}, nil
}

func (l *Lab) ledgerPath() string { return filepath.Join(l.StateDir, "devices.json") }
func (l *Lab) leasesDir() string  { return filepath.Join(l.StateDir, "leases") }
func (l *Lab) locksDir() string   { return filepath.Join(l.StateDir, "locks") }

func (l *Lab) leasePath(id string) string { return filepath.Join(l.leasesDir(), id+".json") }

func (l *Lab) now() time.Time { return l.Now().UTC().Truncate(time.Second) }

// scrubbedEnv is the child environment: ENABLE_GO_IOS_AGENT is dropped
// because go-ios answers it by spawning `ios tunnel start --userspace` as a
// detached daemon (reparented to launchd), which devlab must never leave
// behind.
func scrubbedEnv() []string {
	env := os.Environ()
	kept := env[:0:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, "ENABLE_GO_IOS_AGENT=") {
			continue
		}
		kept = append(kept, kv)
	}
	return kept
}

// RealExec runs commands on this machine, each in its own process group so
// a timeout stops the whole tree it started and nothing else.
func RealExec(ctx context.Context, c Cmd) (CmdOut, error) {
	if len(c.Args) == 0 {
		return CmdOut{}, errors.New("empty command")
	}
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, c.Args[0], c.Args[1:]...)
	cmd.Env = scrubbedEnv()
	ownGroup(cmd)
	var stdout, stderr bytes.Buffer
	if c.Stdout != nil {
		cmd.Stdout = c.Stdout
	} else {
		cmd.Stdout = &stdout
	}
	cmd.Stderr = &stderr
	err := cmd.Run()
	out := CmdOut{Stdout: stdout.String(), Stderr: stderr.String()}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && ctx.Err() == nil {
		out.Code = exitErr.ExitCode()
		return out, nil
	}
	if ctx.Err() != nil {
		return out, ctx.Err()
	}
	return out, err
}

func httpGet(ctx context.Context, url string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New(resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 4<<20))
}

// run executes one command through Lab.Exec.
func (l *Lab) run(ctx context.Context, timeout time.Duration, args ...string) (CmdOut, error) {
	return l.Exec(ctx, Cmd{Args: args, Timeout: timeout})
}

// stderrTail keeps a failure message short: the last non-empty line.
func stderrTail(out CmdOut) string {
	lines := strings.Split(strings.TrimSpace(out.Stderr+"\n"+out.Stdout), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			if len(s) > 300 {
				s = s[:300]
			}
			return s
		}
	}
	return "exit " + strconv.Itoa(out.Code)
}

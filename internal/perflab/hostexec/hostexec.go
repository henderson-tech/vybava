// Package hostexec is how perflab's preflight, network and WDA verbs reach
// the Mac: one external-process seam (each child in its own process group,
// a hard timeout and an optional no-output stall watchdog that stop exactly
// that group, never anything found by name), the kernel pipe capacity probe
// that predicts an xcodebuild "Planning build" hang, and the `perflab[...]`
// progress lines Monitor greps on stderr.
//
// Tests substitute a Runner that replays recorded transcripts, so no test
// reaches a device or Xcode.
package hostexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Cmd is one external process.
type Cmd struct {
	Argv []string
	Dir  string
	// Env entries (KEY=VALUE) override the base environment by name.
	Env []string
	// Log, when set, receives stdout and stderr as they arrive (build
	// logs); Result then keeps only the last TailBytes of the output.
	Log io.Writer
	// Timeout is the hard limit; 0 means none.
	Timeout time.Duration
	// Stall stops the group when no output arrives for this long; 0 means
	// no watchdog.
	Stall time.Duration
	// Unset names variables removed from the base environment before Env
	// applies (an adapter's runner.unset).
	Unset []string
	// Stdin feeds the process (a Perfetto config over `perfetto -c -`).
	Stdin io.Reader
	// Started, when set, receives the child's pid (its own process group)
	// once it runs, so a lease can record it for the SessionEnd release.
	Started func(pid int)
}

// Result is a finished process. A non-zero Exit is not an error: Run
// returns an error only when the process could not start.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	Exit     int
	Stalled  bool
	TimedOut bool
	Duration time.Duration
}

// Combined is stdout then stderr as one string.
func (r Result) Combined() string {
	return strings.TrimSpace(string(r.Stdout) + "\n" + string(r.Stderr))
}

// Tail is the last non-empty output line, or "exit N" when there is none;
// it keeps a failure detail short.
func (r Result) Tail() string {
	lines := strings.Split(r.Combined(), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if s := strings.TrimSpace(lines[i]); s != "" {
			return s
		}
	}
	return fmt.Sprintf("exit %d", r.Exit)
}

// Runner runs external processes.
type Runner interface {
	Run(ctx context.Context, c Cmd) (Result, error)
}

// TailBytes is how much output a logged command keeps in memory for the
// diagnostic detail; the full output is in the log.
const TailBytes = 16 << 10

const (
	// PipeCapacityLow is the floor below which xcodebuild hangs: a fresh
	// pipe that buffers less than 16 KiB means the kernel's pipe memory is
	// exhausted (it measured 512 B during the 4 h "Planning build" hang).
	PipeCapacityLow = 16 << 10
	// PipeProbeCap stops the probe once a pipe has proven ample.
	PipeProbeCap = 1 << 20
)

// OS is the real Runner. BaseEnv defaults to os.Environ().
type OS struct {
	BaseEnv []string
}

// Run starts c in its own process group and waits for it, enforcing the
// timeout and the stall watchdog.
func (r OS) Run(ctx context.Context, c Cmd) (Result, error) {
	if len(c.Argv) == 0 {
		return Result{}, errors.New("empty command")
	}
	base := r.BaseEnv
	if base == nil {
		base = os.Environ()
	}
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	cmd.Dir = c.Dir
	if len(c.Unset) > 0 {
		kept := make([]string, 0, len(base))
		for _, kv := range base {
			if k, _, _ := strings.Cut(kv, "="); !slices.Contains(c.Unset, k) {
				kept = append(kept, kv)
			}
		}
		base = kept
	}
	cmd.Env = MergeEnv(base, c.Env)
	cmd.Stdin = c.Stdin
	setProcessGroup(cmd)
	cmd.Cancel = func() error { return stopGroup(cmd) }
	cmd.WaitDelay = 10 * time.Second

	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	var stdout, stderr bytes.Buffer
	var tail tailBuffer
	if c.Log != nil {
		w := &activityWriter{w: io.MultiWriter(&lockedWriter{w: c.Log}, &tail), last: &last}
		cmd.Stdout, cmd.Stderr = w, w
	} else {
		cmd.Stdout = &activityWriter{w: &stdout, last: &last}
		cmd.Stderr = &activityWriter{w: &stderr, last: &last}
	}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return Result{Exit: -1}, err
	}
	if c.Started != nil {
		c.Started(cmd.Process.Pid)
	}
	var stalled atomic.Bool
	done := make(chan struct{})
	if c.Stall > 0 {
		go watchStall(c.Stall, &last, &stalled, cancel, done)
	}
	waitErr := cmd.Wait()
	close(done)

	res := Result{Duration: time.Since(start), Stalled: stalled.Load()}
	if c.Log != nil {
		res.Stdout = tail.Bytes()
	} else {
		res.Stdout, res.Stderr = stdout.Bytes(), stderr.Bytes()
	}
	res.TimedOut = !res.Stalled && errors.Is(ctx.Err(), context.DeadlineExceeded)
	var exitErr *exec.ExitError
	switch {
	case waitErr == nil:
		res.Exit = 0
	case errors.As(waitErr, &exitErr):
		res.Exit = exitErr.ExitCode()
	default:
		res.Exit = -1
	}
	return res, nil
}

// NotFound reports whether a Run error means the binary is not installed.
func NotFound(err error) bool {
	return errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist)
}

func watchStall(stall time.Duration, last *atomic.Int64, stalled *atomic.Bool, cancel context.CancelFunc, done <-chan struct{}) {
	tick := min(max(stall/10, 10*time.Millisecond), time.Second)
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case now := <-t.C:
			if now.Sub(time.Unix(0, last.Load())) >= stall {
				stalled.Store(true)
				cancel()
				return
			}
		}
	}
}

// MergeEnv overrides base entries by name with add.
func MergeEnv(base, add []string) []string {
	drop := map[string]bool{}
	for _, kv := range add {
		if k, _, ok := strings.Cut(kv, "="); ok {
			drop[k] = true
		}
	}
	out := make([]string, 0, len(base)+len(add))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if !drop[k] {
			out = append(out, kv)
		}
	}
	return append(out, add...)
}

type activityWriter struct {
	w    io.Writer
	last *atomic.Int64
}

func (a *activityWriter) Write(p []byte) (int, error) {
	a.last.Store(time.Now().UnixNano())
	return a.w.Write(p)
}

// tailBuffer keeps the last TailBytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > TailBytes {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-TailBytes:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) Bytes() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]byte(nil), t.buf...)
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// Progress prints the stderr lines Monitor anchors on (`^perflab\[`): one
// per phase change, plus a heartbeat while a phase runs long. stdout carries
// only the envelope, so progress never goes there.
type Progress struct {
	W     io.Writer
	Tag   string // "wda build", "net s20", "doctor iphone11"
	Now   func() time.Time
	mu    sync.Mutex
	start time.Time
	phase string
}

// NewProgress starts the clock; a nil writer discards.
func NewProgress(w io.Writer, tag string, now func() time.Time) *Progress {
	if w == nil {
		w = io.Discard
	}
	if now == nil {
		now = time.Now
	}
	return &Progress{W: w, Tag: tag, Now: now, start: now()}
}

// Phase records a phase change: `perflab[wda build] phase=xcodebuild +42s`.
func (p *Progress) Phase(phase string, kv ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.phase = phase
	p.line("phase=" + phase + joinKV(kv))
}

// Still prints one heartbeat line for the current phase:
// `perflab[net s20] still phase=forwarding open=1 +95s`.
func (p *Progress) Still(kv ...string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.line("still phase=" + p.phase + joinKV(kv))
}

// Heartbeat repeats `still phase=<current>` every interval until ctx ends.
func (p *Progress) Heartbeat(ctx context.Context, every time.Duration, kv func() []string) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			p.mu.Lock()
			extra := ""
			if kv != nil {
				extra = joinKV(kv())
			}
			p.line("still phase=" + p.phase + extra)
			p.mu.Unlock()
		}
	}
}

func (p *Progress) line(body string) {
	elapsed := p.Now().Sub(p.start).Round(time.Second)
	fmt.Fprintf(p.W, "perflab[%s] %s +%ds\n", p.Tag, body, int(elapsed.Seconds()))
}

func joinKV(kv []string) string {
	if len(kv) == 0 {
		return ""
	}
	return " " + strings.Join(kv, " ")
}

package gitkit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
)

// Verb is a script ported to Go: it receives the arguments after the verb
// and the process's stdio, and returns the exit code the script would have.
// A native verb keeps its TypeScript twin's argv grammar, stdout, stderr
// notes and exit codes exactly — skills cannot tell which one ran.
type Verb func(args []string, stdout, stderr io.Writer) int

// native is the registry of ported verbs. A verb listed here runs in-process;
// every other script still execs node on its embedded .ts file.
var native = map[string]Verb{
	"tdd-classify":   runTDDClassify,
	"classify-paths": runClassifyPaths,
	"worktree":       runWorktree,
	"sync-context":   runSyncContext,
	"before-review":  runBeforeReview,
	"list-prs":       runListPRs,
	"merge-precheck": runMergePrecheck,
}

// Native returns the in-process implementation of a verb, if it has one.
func Native(name string) (Verb, bool) {
	verb, ok := native[name]
	return verb, ok
}

// fail reports an error the way every script's top-level catch does.
func fail(stderr io.Writer, err error) int {
	fmt.Fprintf(stderr, "error: %s\n", err.Error())
	return 1
}

// writeJSON matches JSON.stringify(v, null, 2) + "\n": two-space indent,
// no HTML escaping.
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// commandError mirrors Node's execFileSync failure message.
type commandError struct {
	argv   []string
	stderr string
	err    error
}

func (e *commandError) Error() string {
	msg := "Command failed: " + strings.Join(e.argv, " ")
	if e.stderr != "" {
		msg += "\n" + e.stderr
	}
	return msg
}

func (e *commandError) Unwrap() error { return e.err }

// execOpts mirrors the execFileSync options the scripts pass.
type execOpts struct {
	dir string
	// echo, when set, receives the child's stderr — Node's default stdio
	// does this; scripts passing an explicit stdio leave it nil.
	echo    io.Writer
	timeout time.Duration
	// maxBuffer caps stdout and stderr each; 0 is Node's 1 MiB default.
	maxBuffer int
}

// nodeMaxBuffer is execFileSync's default maxBuffer.
const nodeMaxBuffer = 1024 * 1024

// cappedBuffer collects output up to limit; past it, it records the
// overflow and kills the child, as Node does before throwing ENOBUFS.
type cappedBuffer struct {
	buf      bytes.Buffer // not embedded: its ReadFrom would bypass Write
	limit    int
	overflow bool
	kill     func()
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.overflow {
		return len(p), nil
	}
	if b.buf.Len()+len(p) > b.limit {
		b.overflow = true
		b.kill()
		return len(p), nil
	}
	return b.buf.Write(p)
}

// execFile runs name with args and returns stdout, failing the way Node's
// execFileSync does: "Command failed: <argv>\n<stderr>", "spawnSync <name>
// ENOENT" / "ETIMEDOUT" / "ENOBUFS".
func execFile(o execOpts, name string, args ...string) (string, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if o.timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, o.timeout)
		defer cancel()
	}
	limit := o.maxBuffer
	if limit == 0 {
		limit = nodeMaxBuffer
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	// A killed git can leave a grandchild (ssh, a credential helper) holding
	// the output pipes; without a WaitDelay, Wait blocks until it exits.
	cmd.WaitDelay = 2 * time.Second
	cmd.Dir = o.dir
	stdout := &cappedBuffer{limit: limit, kill: cancel}
	stderr := &cappedBuffer{limit: limit, kill: cancel}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if o.echo != nil {
		_, _ = o.echo.Write(stderr.buf.Bytes())
	}
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return "", fmt.Errorf("spawnSync %s ENOENT", name)
	case stdout.overflow || stderr.overflow:
		return "", fmt.Errorf("spawnSync %s ENOBUFS", name)
	case err != nil && ctx.Err() == context.DeadlineExceeded:
		return "", fmt.Errorf("spawnSync %s ETIMEDOUT", name)
	case err != nil:
		return stdout.buf.String(), &commandError{argv: append([]string{name}, args...), stderr: stderr.buf.String(), err: err}
	}
	return stdout.buf.String(), nil
}

var jsDecimal = regexp.MustCompile(`^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$`)

// jsNumber is JavaScript's Number(s) for the forms a CLI argument takes:
// surrounding whitespace ignored, "" is 0, 0x/0o/0b prefixes, decimal and
// exponent notation, ±Infinity. ok is false where Number would be NaN.
func jsNumber(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, true
	}
	if len(s) > 2 && s[0] == '0' {
		base := map[byte]int{'x': 16, 'X': 16, 'o': 8, 'O': 8, 'b': 2, 'B': 2}[s[1]]
		if base != 0 {
			n, err := strconv.ParseUint(s[2:], base, 64)
			return float64(n), err == nil
		}
	}
	switch s {
	case "Infinity", "+Infinity":
		return math.Inf(1), true
	case "-Infinity":
		return math.Inf(-1), true
	}
	if !jsDecimal.MatchString(s) {
		return 0, false
	}
	n, err := strconv.ParseFloat(s, 64)
	return n, err == nil || errors.Is(err, strconv.ErrRange)
}

// positiveInt is `Number.isInteger(n) && n > 0` over jsNumber, bounded to the
// exactly representable integers (2^53) so the result formats as JS would.
func positiveInt(s string) (int, bool) {
	n, ok := jsNumber(s)
	if !ok || n <= 0 || n != math.Trunc(n) || n > 1<<53 {
		return 0, false
	}
	return int(n), true
}

var jsIntPrefix = regexp.MustCompile(`^[+-]?\d+`)

// jsParseInt is Number.parseInt(s, 10): leading whitespace skipped, the
// longest signed digit prefix parsed; ok is false where it returns NaN.
func jsParseInt(s string) (int, bool) {
	m := jsIntPrefix.FindString(strings.TrimLeftFunc(s, unicode.IsSpace))
	if m == "" {
		return 0, false
	}
	n, err := strconv.Atoi(m)
	return n, err == nil
}

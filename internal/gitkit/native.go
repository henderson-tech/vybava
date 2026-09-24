package gitkit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
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

// runIn runs name with args in dir and returns stdout. On failure the
// child's stderr is carried in the error (Node's execFileSync shape).
func runIn(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), &commandError{argv: append([]string{name}, args...), stderr: stderr.String(), err: err}
	}
	return stdout.String(), nil
}

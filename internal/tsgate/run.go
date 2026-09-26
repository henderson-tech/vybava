package tsgate

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Options tune one program's run.
type Options struct {
	Dependencies []string // TS 7 dependency names to try; DefaultTS7Dependencies when empty
	Build        bool     // `-b` on the real tsconfig instead of `-p … --noEmit`
	Args         []string // extra compiler arguments
	Stdout       io.Writer
	Stderr       io.Writer
}

// Run typechecks one program on TS 7 and returns the compiler's exit code.
func Run(tsconfig string, opts Options) (int, error) {
	plan, err := PlanProgram(tsconfig)
	if err != nil {
		return 0, err
	}
	compiler, err := FindTS7(filepath.Dir(plan.Tsconfig), opts.Dependencies)
	if err != nil {
		return 0, err
	}
	args, err := compilerArgs(plan, opts)
	if err != nil {
		return 0, err
	}
	if plan.Derive {
		if err := writeDerived(plan); err != nil {
			return 0, err
		}
	}
	return execCompiler(compiler, args, opts.Stdout, opts.Stderr)
}

func compilerArgs(plan Plan, opts Options) ([]string, error) {
	if opts.Build {
		if plan.Derive {
			return nil, fmt.Errorf("%s: -b builds the real tsconfig, and TS 7 refuses it:\n  %s\nmove these options in the tsconfig, or typecheck without -b",
				plan.Tsconfig, strings.Join(plan.Reasons, "\n  "))
		}
		return append([]string{"-b", plan.Tsconfig}, opts.Args...), nil
	}
	return append([]string{"-p", plan.Target, "--noEmit"}, opts.Args...), nil
}

// execCompiler runs a compiler in the caller's directory, so it prints paths
// the way a bare `tsc` there would, and returns its exit code.
func execCompiler(c Compiler, args []string, stdout, stderr io.Writer) (int, error) {
	cmd := exec.Command(c.Command[0], append(c.Command[1:], args...)...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &exitErr) && exitErr.ExitCode() >= 0:
		return exitErr.ExitCode(), nil
	case errors.As(err, &exitErr):
		return 0, fmt.Errorf("%s %s: %s", c.Package, c.Version, exitErr.ProcessState)
	default:
		return 0, err
	}
}

// Diagnostic is one compiler error, as `--pretty false` prints it.
type Diagnostic struct {
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
	Col     int    `json:"col,omitempty"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (d Diagnostic) key() string {
	return fmt.Sprintf("%s(%d,%d) %s", d.File, d.Line, d.Col, d.Code)
}

func (d Diagnostic) String() string {
	msg, _, _ := strings.Cut(d.Message, "\n")
	if d.File == "" {
		return d.Code + ": " + msg
	}
	return d.key() + ": " + msg
}

// Side is one compiler's run in a parity check.
type Side struct {
	Compiler    Compiler `json:"compiler"`
	Config      string   `json:"config"`
	Exit        int      `json:"exit"`
	WallMS      int64    `json:"wallMs"`
	Diagnostics int      `json:"diagnostics"`
}

// ParityReport compares the classic compiler on the real tsconfig with TS 7
// on what tsgate runs. Diagnostics are matched by file, position and code:
// wording may differ between versions, a location or code may not.
type ParityReport struct {
	Tsconfig     string       `json:"tsconfig"`
	Baseline     Side         `json:"baseline"`
	TS7          Side         `json:"ts7"`
	Same         bool         `json:"same"`
	OnlyBaseline []Diagnostic `json:"onlyBaseline"`
	OnlyTS7      []Diagnostic `json:"onlyTs7"`
}

// Parity runs both compilers on one program, the classic one first.
func Parity(tsconfig string, opts Options) (ParityReport, error) {
	plan, err := PlanProgram(tsconfig)
	if err != nil {
		return ParityReport{}, err
	}
	dir := filepath.Dir(plan.Tsconfig)
	baseline, err := FindBaseline(dir)
	if err != nil {
		return ParityReport{}, err
	}
	ts7, err := FindTS7(dir, opts.Dependencies)
	if err != nil {
		return ParityReport{}, err
	}
	if plan.Derive {
		if err := writeDerived(plan); err != nil {
			return ParityReport{}, err
		}
	}
	report := ParityReport{Tsconfig: plan.Tsconfig, OnlyBaseline: []Diagnostic{}, OnlyTS7: []Diagnostic{}}
	var baseDiags, ts7Diags []Diagnostic
	report.Baseline, baseDiags, err = paritySide(baseline, plan.Tsconfig, opts.Args)
	if err != nil {
		return report, err
	}
	report.TS7, ts7Diags, err = paritySide(ts7, plan.Target, opts.Args)
	if err != nil {
		return report, err
	}
	report.OnlyBaseline, report.OnlyTS7 = diffDiagnostics(baseDiags, ts7Diags)
	report.Same = len(report.OnlyBaseline) == 0 && len(report.OnlyTS7) == 0
	return report, nil
}

func paritySide(c Compiler, config string, extra []string) (Side, []Diagnostic, error) {
	var out bytes.Buffer
	args := append([]string{"-p", config, "--noEmit", "--pretty", "false"}, extra...)
	start := time.Now()
	code, err := execCompiler(c, args, &out, &out)
	if err != nil {
		return Side{}, nil, err
	}
	diags := parseDiagnostics(out.String())
	return Side{Compiler: c, Config: config, Exit: code, WallMS: time.Since(start).Milliseconds(), Diagnostics: len(diags)}, diags, nil
}

var (
	locatedDiag = regexp.MustCompile(`^(.+?)\((\d+),(\d+)\): error (TS\d+): (.*)$`)
	globalDiag  = regexp.MustCompile(`^error (TS\d+): (.*)$`)
)

// parseDiagnostics reads `--pretty false` output: one `file(line,col): error
// TSnnnn: message` or `error TSnnnn: message` line per diagnostic, indented
// lines continuing the message before them.
func parseDiagnostics(out string) []Diagnostic {
	var diags []Diagnostic
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		if m := locatedDiag.FindStringSubmatch(line); m != nil {
			l, _ := strconv.Atoi(m[2])
			c, _ := strconv.Atoi(m[3])
			diags = append(diags, Diagnostic{File: filepath.ToSlash(m[1]), Line: l, Col: c, Code: m[4], Message: m[5]})
			continue
		}
		if m := globalDiag.FindStringSubmatch(line); m != nil {
			diags = append(diags, Diagnostic{Code: m[1], Message: m[2]})
			continue
		}
		if len(diags) > 0 && strings.TrimSpace(line) != "" && (line[0] == ' ' || line[0] == '\t') {
			diags[len(diags)-1].Message += "\n" + strings.TrimSpace(line)
		}
	}
	return diags
}

// diffDiagnostics returns what only a and only b report, each sorted, as
// multisets: the same error twice on one side and once on the other differs.
func diffDiagnostics(a, b []Diagnostic) (onlyA, onlyB []Diagnostic) {
	count := map[string]int{}
	for _, d := range b {
		count[d.key()]++
	}
	onlyA = []Diagnostic{}
	for _, d := range a {
		if count[d.key()] > 0 {
			count[d.key()]--
			continue
		}
		onlyA = append(onlyA, d)
	}
	countA := map[string]int{}
	for _, d := range a {
		countA[d.key()]++
	}
	onlyB = []Diagnostic{}
	for _, d := range b {
		if countA[d.key()] > 0 {
			countA[d.key()]--
			continue
		}
		onlyB = append(onlyB, d)
	}
	byKey := func(list []Diagnostic) {
		sort.SliceStable(list, func(i, j int) bool { return list[i].key() < list[j].key() })
	}
	byKey(onlyA)
	byKey(onlyB)
	return onlyA, onlyB
}

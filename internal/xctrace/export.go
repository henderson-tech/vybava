package xctrace

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// Runner runs one external command to completion. exit is the process's
// exit code; err is set only when the command could not run at all (not
// found, killed by the context).
type Runner func(ctx context.Context, name string, args ...string) (stdout, stderr []byte, exit int, err error)

// ExecRunner is the Runner over os/exec.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, []byte, int, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && ctx.Err() == nil {
		return stdout.Bytes(), stderr.Bytes(), exitErr.ExitCode(), nil
	}
	if err != nil {
		return stdout.Bytes(), stderr.Bytes(), -1, err
	}
	return stdout.Bytes(), stderr.Bytes(), 0, nil
}

// Exporter runs `xcrun xctrace export` and caches each export beside the
// trace as `<trace>.toc.xml` and `<trace>.<schema>.xml`. A cache is used only
// when it is exactly the export asked for (one table, that schema), so a
// scratch tool's file of the same name is re-exported, never trusted.
type Exporter struct {
	Run Runner
	// Reread ignores every cache and exports again.
	Reread bool
}

// CachePath is where the export named kind ("toc" or a schema) of trace lives.
func CachePath(trace, kind string) string {
	return strings.TrimRight(trace, string(filepath.Separator)) + "." + kind + ".xml"
}

// TOC exports (or reads the cached) table of contents.
func (x Exporter) TOC(ctx context.Context, trace string) (TOC, error) {
	path := CachePath(trace, "toc")
	if !x.Reread && validTOC(path) {
		return readTOCFile(path)
	}
	if err := x.export(ctx, trace, path, "--toc"); err != nil {
		return TOC{}, err
	}
	if !validTOC(path) {
		return TOC{}, diag(DiagTraceUnreadable, fmt.Sprintf("xctrace export --toc of %s printed no <trace-toc> run", trace), reRecordFix)
	}
	return readTOCFile(path)
}

// Table exports (or reuses the cached) table of run 1 named schema and
// returns the file to stream with ReadTable.
func (x Exporter) Table(ctx context.Context, trace, schema string) (string, error) {
	path := CachePath(trace, schema)
	if !x.Reread && validTable(path, schema) {
		return path, nil
	}
	xpath := fmt.Sprintf(`/trace-toc/run[@number="1"]/data/table[@schema="%s"]`, schema)
	if err := x.export(ctx, trace, path, "--xpath", xpath); err != nil {
		return "", err
	}
	if !validTable(path, schema) {
		os.Remove(path)
		return "", diag(DiagTraceUnreadable, fmt.Sprintf("xctrace export of the %s table of %s printed no such table", schema, trace), reRecordFix)
	}
	return path, nil
}

func (x Exporter) export(ctx context.Context, trace, dest string, args ...string) error {
	if _, err := os.Stat(trace); err != nil {
		return diag(DiagTraceUnreadable, fmt.Sprintf("cannot read %s: %v", trace, err), "pass the .trace bundle xctrace recorded")
	}
	run := x.Run
	if run == nil {
		run = ExecRunner
	}
	full := append([]string{"xctrace", "export", "--input", trace}, args...)
	stdout, stderr, exit, err := run(ctx, "xcrun", full...)
	if err != nil {
		if ctx.Err() != nil {
			return diag(DiagTraceUnreadable, fmt.Sprintf("xctrace export of %s did not finish: %v", trace, ctx.Err()), "re-run with a longer --timeout")
		}
		return diag(DiagToolMissing, fmt.Sprintf("cannot run xcrun xctrace: %v", err), "xcode-select --install, or select Xcode with sudo xcode-select -s /Applications/Xcode.app")
	}
	if exit != 0 {
		msg := strings.TrimSpace(string(stderr))
		if msg == "" {
			msg = strings.TrimSpace(string(stdout))
		}
		if i := strings.IndexByte(msg, '\n'); i >= 0 {
			msg = msg[:i]
		}
		return diag(DiagTraceUnreadable, fmt.Sprintf("xctrace export of %s failed (exit %d): %s", trace, exit, msg), reRecordFix)
	}
	tmp := dest + ".tmp"
	if err := os.WriteFile(tmp, stdout, 0o644); err != nil {
		return fmt.Errorf("cache the export: %w", err)
	}
	return os.Rename(tmp, dest)
}

func readTOCFile(path string) (TOC, error) {
	fh, err := os.Open(path)
	if err != nil {
		return TOC{}, fmt.Errorf("read %s: %w", path, err)
	}
	defer fh.Close()
	toc, err := ParseTOC(fh)
	if err != nil {
		return TOC{}, diag(DiagTraceUnreadable, fmt.Sprintf("%s: %v", path, err), reRecordFix)
	}
	return toc, nil
}

func validTOC(path string) bool {
	fh, err := os.Open(path)
	if err != nil {
		return false
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		if strings.Contains(sc.Text(), "<run number=") {
			return true
		}
	}
	return false
}

var schemaNameRe = regexp.MustCompile(`<schema name="([^"]+)"`)

// validTable: the file holds at least one <schema> and every one of them is
// the requested table (an empty <trace-query-result> or a file holding
// several tables is never a cache of this one).
func validTable(path, schema string) bool {
	fh, err := os.Open(path)
	if err != nil {
		return false
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 64<<10), 64<<20)
	found := false
	for sc.Scan() {
		for _, m := range schemaNameRe.FindAllStringSubmatch(sc.Text(), -1) {
			if m[1] != schema {
				return false
			}
			found = true
		}
	}
	return found && sc.Err() == nil
}

var runErrorRe = regexp.MustCompile(`(?m)^(?:\[\d+-\d+\] )?[^\S\n]*\*[^\S\n]*\[Error\][^\S\n]*(.+)$`)

// RunErrors are the `* [Error] <message>` lines `xctrace record` printed
// under "Run issues were detected (trace is still ready to be viewed):".
func RunErrors(recordOutput string) []string {
	var out []string
	for _, m := range runErrorRe.FindAllStringSubmatch(recordOutput, -1) {
		out = append(out, strings.TrimSpace(m[1]))
	}
	return out
}

// HitchesUnsupported reports xctrace's refusal to record hitches (a
// simulator): the saved trace is empty and must fail, not read as zero.
func HitchesUnsupported(recordOutput string) bool {
	return strings.Contains(recordOutput, "Hitches is not supported")
}

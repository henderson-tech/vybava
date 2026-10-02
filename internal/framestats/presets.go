package framestats

import (
	"context"
	"embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// The SQL presets are the drill-downs of the S20 render kits (PR #1770 and
// the calendar campaign), run through trace_processor_shell when the native
// reading is not enough. Each takes {{package}} and {{vsync_ns}}.
//
//go:embed sql/*.sql
var presetFS embed.FS

// DiagPresetUnknown: --sql named no embedded preset; the fix lists them.
const DiagPresetUnknown = "PRESET_UNKNOWN"

// DiagToolMissing: trace_processor_shell is not installed (or
// PERFLAB_TRACE_PROCESSOR points nowhere); the fix installs it.
const DiagToolMissing = "TOOL_MISSING"

// DiagPresetFailed: trace_processor_shell exited non-zero on a preset; the
// detail carries its first error line.
const DiagPresetFailed = "PRESET_FAILED"

// Presets lists the embedded preset names.
func Presets() []string {
	entries, _ := presetFS.ReadDir("sql")
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, strings.TrimSuffix(e.Name(), ".sql"))
	}
	sort.Strings(out)
	return out
}

// PresetParams fill a preset's placeholders.
type PresetParams struct {
	Package       string
	VsyncPeriodNs int64
}

// RenderPreset returns the preset's SQL with its placeholders filled (the
// period defaults to 120 Hz, the S20 kit's).
func RenderPreset(name string, p PresetParams) (string, error) {
	raw, err := presetFS.ReadFile("sql/" + name + ".sql")
	if err != nil {
		return "", diag(DiagPresetUnknown, fmt.Sprintf("no SQL preset %q", name),
			"perflab analyze <trace.pftrace> --sql <"+strings.Join(Presets(), "|")+">")
	}
	if p.Package == "" || strings.ContainsAny(p.Package, "'\"\\") {
		return "", diag(DiagUsage, fmt.Sprintf("preset %s needs a plain package name, got %q", name, p.Package),
			"perflab analyze <trace.pftrace> --sql "+name+" --package <app id>")
	}
	period := p.VsyncPeriodNs
	if period <= 0 {
		period = 8_333_333
	}
	return strings.NewReplacer("{{package}}", p.Package, "{{vsync_ns}}", strconv.FormatInt(period, 10)).Replace(string(raw)), nil
}

// TraceProcessor is the trace_processor_shell binary perflab runs:
// PERFLAB_TRACE_PROCESSOR, else the first of trace_processor_shell and
// trace_processor on PATH.
func TraceProcessor() (string, error) {
	if p := os.Getenv("PERFLAB_TRACE_PROCESSOR"); p != "" {
		if _, err := os.Stat(p); err != nil {
			return "", diag(DiagToolMissing, fmt.Sprintf("PERFLAB_TRACE_PROCESSOR=%s: %v", p, err), traceProcessorFix)
		}
		return p, nil
	}
	for _, name := range []string{"trace_processor_shell", "trace_processor"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", diag(DiagToolMissing, "trace_processor_shell is not on PATH", traceProcessorFix)
}

const traceProcessorFix = "curl -LO https://get.perfetto.dev/trace_processor && chmod +x trace_processor && mv trace_processor ~/.local/bin/ (or set PERFLAB_TRACE_PROCESSOR)"

// Runner runs one command to completion; exit is its exit code, err is set
// only when it could not run at all.
type Runner func(ctx context.Context, name string, args ...string) (stdout, stderr []byte, exit int, err error)

// RunPreset renders a preset, writes it beside nothing the caller owns (a
// temp dir) and runs it over trace; the output is trace_processor's CSV-ish
// text, returned as is.
func RunPreset(ctx context.Context, run Runner, tool, trace, name string, p PresetParams) (string, error) {
	sql, err := RenderPreset(name, p)
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "perflab-sql-")
	if err != nil {
		return "", fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, name+".sql")
	if err := os.WriteFile(file, []byte(sql), 0o600); err != nil {
		return "", fmt.Errorf("write %s: %w", file, err)
	}
	stdout, stderr, exit, err := run(ctx, tool, "-q", file, trace)
	if err != nil {
		return "", diag(DiagToolMissing, fmt.Sprintf("cannot run %s: %v", tool, err), traceProcessorFix)
	}
	if exit != 0 {
		msg := strings.TrimSpace(string(stderr))
		if i := strings.IndexByte(msg, '\n'); i >= 0 {
			msg = msg[:i]
		}
		return "", diag(DiagPresetFailed, fmt.Sprintf("%s -q %s.sql exited %d: %s", filepath.Base(tool), name, exit, msg),
			"check the trace was recorded with the probe config (atrace gfx, view; sched; frametimeline), then perflab analyze "+trace+" --sql "+name)
	}
	return string(stdout), nil
}

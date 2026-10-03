package fleet

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/henderson-tech/vybava/internal/runx"
)

// maxRecordBytes bounds one registry file. A record is a few hundred bytes;
// anything larger is not a record.
const maxRecordBytes = 1 << 20

// record is one ~/.claude/sessions/<pid>.json as Claude Code writes it. The
// format is undocumented, so readRegistry checks the fields this reader
// relies on before trusting a file.
type record struct {
	SessionID       string `json:"sessionId"`
	PID             int    `json:"pid"`
	PIDDomain       string `json:"pidDomain"`
	ProcStart       string `json:"procStart"`
	StartedAt       int64  `json:"startedAt"`
	Status          string `json:"status"`
	StatusUpdatedAt int64  `json:"statusUpdatedAt"`
	WaitingFor      string `json:"waitingFor"`
	Kind            string `json:"kind"`
	Entrypoint      string `json:"entrypoint"`
	Version         string `json:"version"`
	Name            string `json:"name"`
	CWD             string `json:"cwd"`
}

type fieldKind int

const (
	kindString fieldKind = iota
	kindNumber
)

// required are the fields every record must carry with the right JSON type.
// Missing any one means the file is not a record this reader understands.
var required = []struct {
	name string
	kind fieldKind
}{
	{"sessionId", kindString},
	{"pid", kindNumber},
	{"status", kindString},
	{"cwd", kindString},
	{"procStart", kindString},
	{"statusUpdatedAt", kindNumber},
}

// readRegistry reads every *.json record. Only *.json is opened: the
// directory also holds per-session .key files, which are never read. One bad
// file is skipped with a warning naming the file and field; when files exist
// and NONE of them has the expected shape, the format has changed under us
// and the read fails loudly instead of reporting an empty fleet.
func readRegistry(dir string) ([]record, []runx.Diagnostic, error) {
	var diags []runx.Diagnostic
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, nil, err
	}
	if len(paths) == 0 {
		if _, statErr := os.Stat(dir); errors.Is(statErr, fs.ErrNotExist) {
			return nil, []runx.Diagnostic{warning(DiagRegistryMissing, dir+" does not exist; no Claude Code session has registered on this machine")}, nil
		}
	}
	sort.Strings(paths)

	records := make([]record, 0, len(paths))
	reasons := map[string]int{}
	for _, path := range paths {
		rec, reason := readRecord(path)
		if reason != "" {
			reasons[reason]++
			diags = append(diags, warning(DiagRegistryFileSkipped, filepath.Base(path)+": "+reason))
			continue
		}
		records = append(records, rec)
	}
	if len(paths) > 0 && len(records) == 0 {
		return nil, diags, runx.DiagError{Diag: runx.Diagnostic{
			Code: DiagRegistryShapeUnknown, Severity: "error",
			Detail: fmt.Sprintf("none of the %d files in %s has the registry shape fleet reads (most common: %s) — Claude Code changed its session registry format",
				len(paths), dir, mostCommon(reasons)),
			Fix: "update vybava (vybava upgrade); if current, the registry reader in internal/fleet needs the new format",
		}}
	}
	return records, diags, nil
}

// readRecord returns a record, or the reason the file is not one.
func readRecord(path string) (record, string) {
	info, err := os.Stat(path)
	if err != nil {
		return record{}, "unreadable: " + err.Error()
	}
	if !info.Mode().IsRegular() {
		return record{}, "not a regular file"
	}
	if info.Size() > maxRecordBytes {
		return record{}, fmt.Sprintf("%d bytes, larger than any record", info.Size())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return record{}, "unreadable: " + err.Error()
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return record{}, "not a JSON object"
	}
	for _, field := range required {
		value, ok := fields[field.name]
		if !ok {
			return record{}, fmt.Sprintf("field %q missing", field.name)
		}
		if !hasKind(value, field.kind) {
			kind := "string"
			if field.kind == kindNumber {
				kind = "number"
			}
			return record{}, fmt.Sprintf("field %q is not a %s", field.name, kind)
		}
	}
	var rec record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return record{}, "unexpected field type: " + err.Error()
	}
	if rec.SessionID == "" {
		return record{}, `field "sessionId" is empty`
	}
	if rec.PID <= 0 {
		return record{}, `field "pid" is not a process id`
	}
	return rec, ""
}

func hasKind(value json.RawMessage, kind fieldKind) bool {
	trimmed := strings.TrimSpace(string(value))
	if trimmed == "" {
		return false
	}
	switch kind {
	case kindString:
		return trimmed[0] == '"'
	default:
		return trimmed[0] == '-' || trimmed[0] >= '0' && trimmed[0] <= '9'
	}
}

func mostCommon(reasons map[string]int) string {
	best, count := "", 0
	for reason, n := range reasons {
		if n > count || n == count && reason < best {
			best, count = reason, n
		}
	}
	return fmt.Sprintf("%s in %d", best, count)
}

// isRepo reports whether dir holds a .git entry.
func isRepo(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))
	return err == nil
}

// worktreeLabel names the linked worktree a directory sits in, relative to
// the repository's worktree folder ("feat/claude-mods-vt-4759"); "" in the
// main checkout. A removed worktree is labelled lexically from its path.
func worktreeLabel(cwd, root string) string {
	if cwd == "" || root == "" {
		return ""
	}
	cwd, root = filepath.Clean(cwd), filepath.Clean(root)
	for dir := cwd; dir != root && strings.HasPrefix(dir, root+string(filepath.Separator)); dir = filepath.Dir(dir) {
		if info, err := os.Lstat(filepath.Join(dir, ".git")); err == nil && info.Mode().IsRegular() {
			label, _ := trimWorktreeDir(dir, root)
			return label
		}
	}
	if _, err := os.Stat(cwd); err != nil {
		if label, held := trimWorktreeDir(cwd, root); held {
			return label
		}
	}
	return ""
}

// trimWorktreeDir is dir relative to root, without the worktree folder;
// held reports whether dir sat inside one.
func trimWorktreeDir(dir, root string) (label string, held bool) {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return "", false
	}
	rel = filepath.ToSlash(rel)
	for _, holder := range []string{".claude/worktrees/", ".worktrees/"} {
		if strings.HasPrefix(rel, holder) {
			return strings.TrimPrefix(rel, holder), true
		}
	}
	return rel, false
}

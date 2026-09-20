package memo

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/claudeguards"
)

// HookPayload is the subset of a Claude Code / Codex hook event memo reads.
type HookPayload struct {
	HookEventName  string `json:"hook_event_name"`
	ToolName       string `json:"tool_name"`
	Cwd            string `json:"cwd"`
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	ToolInput      struct {
		FilePath string `json:"file_path"`
		Path     string `json:"path"`
		Command  string `json:"command"`
	} `json:"tool_input"`
}

// HookResult is what `memo hook` concluded. Refused is set only for a
// PreToolUse that must be blocked; Recorded counts usage events appended by
// a Stop / SessionEnd scan.
type HookResult struct {
	Event    string   `json:"event"`
	Refused  *Diag    `json:"refused,omitempty"`
	Recorded int      `json:"recorded"`
	Homes    []string `json:"homes,omitempty"`
	Rendered []string `json:"rendered,omitempty"`
	Skipped  string   `json:"skipped,omitempty"` // why no home was credited; a Stop is never blocked
}

var (
	rewritingTools = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true}
	ledgerFiles    = map[string]string{LedgerFile: "memo add", IndexFile: "memo render", UsageFile: "memo touch"}
	canonicalHome  = regexp.MustCompile(`/\.(?:claude|codex)/(?:projects/[^/]+/)?memory$`)
	inPlaceFlagRE  = regexp.MustCompile(`^(?:-i|-i\S*|--in-place\S*|-[a-zA-Z]*i[a-zA-Z]*)$`)
)

// ReadHookPayload parses one hook event from stdin.
func ReadHookPayload(r io.Reader) (HookPayload, error) {
	raw, err := io.ReadAll(io.LimitReader(r, 4<<20))
	if err != nil {
		return HookPayload{}, err
	}
	var p HookPayload
	return p, json.Unmarshal(raw, &p)
}

// RunHook dispatches on the event name: PreToolUse guards the ledger files,
// Stop / SessionEnd harvest the transcript. Unknown events are a no-op.
func (e Env) RunHook(p HookPayload, now time.Time) (HookResult, error) {
	res := HookResult{Event: p.HookEventName}
	switch p.HookEventName {
	case "PreToolUse":
		res.Refused = RefuseHandWrite(p)
	case "Stop", "SessionEnd":
		return e.harvest(p, now)
	}
	return res, nil
}

// RefuseHandWrite returns the refusal for a tool call that would rewrite
// LEDGER.md, MEMORY.md or usage.jsonl in a home by hand, or nil.
func RefuseHandWrite(p HookPayload) *Diag {
	var targets []string
	if rewritingTools[p.ToolName] {
		targets = append(targets, p.ToolInput.FilePath, p.ToolInput.Path)
	}
	if p.ToolName == "Bash" {
		targets = append(targets, shellWriteTargets(p.ToolInput.Command)...)
	}
	for _, t := range targets {
		if abs, verb, ok := ledgerTarget(t, p.Cwd); ok {
			return errorDiag(DiagHookRefused, fmt.Sprintf("%s is owned by memo; %s rewrites it by hand and breaks the append-only ledger", abs, p.ToolName), verb+"  # "+filepath.Base(abs)+" is written only by memo")
		}
	}
	return nil
}

// ledgerTarget decides whether a path is a memo-owned file: its basename is
// one of the three and its directory is a home (holds LEDGER.md or is a
// canonical memory home).
func ledgerTarget(path, cwd string) (string, string, bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", "", false
	}
	verb, owned := ledgerFiles[filepath.Base(path)]
	if !owned {
		return "", "", false
	}
	if !filepath.IsAbs(path) && cwd != "" {
		path = filepath.Join(cwd, path)
	}
	path = filepath.Clean(path)
	dir := filepath.Dir(path)
	if !hasLedger(dir) && !canonicalHome.MatchString(filepath.ToSlash(dir)) {
		return "", "", false
	}
	if filepath.Base(path) != LedgerFile && !hasLedger(dir) {
		return "", "", false
	}
	return path, verb, true
}

// shellWriteTargets lists the files a command line writes: redirection
// targets, `tee`, `sed -i`/`perl -i` operands, `cp`/`mv` destinations,
// `rm`/`truncate` operands. Segmentation is claudeguards' one definition.
func shellWriteTargets(command string) []string {
	var out []string
	for _, seg := range claudeguards.Segments(command) {
		fields := claudeguards.ShellFields(seg)
		if len(fields) == 0 {
			continue
		}
		for i, f := range fields {
			if (f == ">" || f == ">>" || f == "1>" || f == "2>" || f == "&>") && i+1 < len(fields) {
				out = append(out, fields[i+1])
			}
			for _, prefix := range []string{">>", ">"} {
				if strings.HasPrefix(f, prefix) && len(f) > len(prefix) && f != ">&2" {
					out = append(out, strings.TrimPrefix(f, prefix))
					break
				}
			}
		}
		switch fields[0] {
		case "tee":
			out = append(out, operands(fields[1:])...)
		case "sed", "perl":
			if hasInPlace(fields[1:]) {
				out = append(out, operands(fields[1:])...)
			}
		case "cp", "mv":
			if ops := operands(fields[1:]); len(ops) > 1 {
				out = append(out, ops[len(ops)-1])
			}
		case "rm", "truncate", "shred":
			out = append(out, operands(fields[1:])...)
		}
	}
	return out
}

func hasInPlace(args []string) bool {
	for _, a := range args {
		if inPlaceFlagRE.MatchString(a) && a != "-" {
			return true
		}
	}
	return false
}

func operands(args []string) []string {
	var out []string
	skipNext := false
	for _, a := range args {
		switch {
		case skipNext:
			skipNext = false
		case a == "-e" || a == "-f" || a == "--expression" || a == "--file":
			skipNext = true
		case strings.HasPrefix(a, "-") && a != "-":
		default:
			out = append(out, a)
		}
	}
	return out
}

// harvest scans the transcript and credits every session home with the rows
// it cited, showed or read; alias-scoped citations credit that alias only.
func (e Env) harvest(p HookPayload, now time.Time) (HookResult, error) {
	res := HookResult{Event: p.HookEventName}
	if p.TranscriptPath == "" {
		return res, nil
	}
	cites, err := ScanTranscript(p.TranscriptPath)
	if os.IsNotExist(err) {
		return res, nil
	}
	if err != nil {
		return res, err
	}
	env := e
	if p.Cwd != "" {
		env.Cwd = p.Cwd
	}
	if p.SessionID != "" {
		env.Session = p.SessionID
	}
	homes, d, err := env.Resolve("", "")
	if err != nil {
		return res, err
	}
	if d != nil {
		if h, ok := env.Enclosing(env.Cwd); ok {
			homes = []Home{h}
		} else {
			res.Skipped = d.Detail
		}
	}
	homes = append(homes, env.aliasHomes(cites, homes)...)
	for _, h := range homes {
		n, rendered, err := env.credit(h, cites, now)
		if err != nil {
			return res, err
		}
		res.Recorded += n
		res.Homes = append(res.Homes, h.Path)
		if rendered {
			res.Rendered = append(res.Rendered, filepath.Join(h.Path, IndexFile))
		}
	}
	return res, nil
}

func (e Env) credit(h Home, c Citations, now time.Time) (int, bool, error) {
	l, d, err := e.Open(h, false)
	if err != nil || d != nil {
		return 0, false, err
	}
	existing, d, err := LoadEvents(h.Path)
	if err != nil || d != nil {
		return 0, false, err
	}
	var incoming []Event
	add := func(id int, kind string) {
		if _, ok := l.Find(id); ok {
			incoming = append(incoming, NewEvent(id, kind, e.Session, now))
		}
	}
	cites, shows := c.Cites, c.Shows
	if l.Kind == KindTeam {
		cites, shows = c.Team, c.TeamShows
	}
	for id := range cites {
		add(id, "cite")
	}
	for id := range c.Scoped[l.Alias] {
		add(id, "cite")
	}
	for id := range shows {
		add(id, "show")
	}
	for _, r := range l.Rows {
		for _, link := range r.Links {
			if m := noteLinkRE.FindStringSubmatch(link); m != nil && c.Reads[filepath.Join(h.Path, NotesDir, m[1]+".md")] {
				add(r.ID, "read")
			}
		}
	}
	added, err := AppendEvents(h.Path, existing, incoming)
	if err != nil || added == 0 {
		return added, false, err
	}
	all, _, err := LoadEvents(h.Path)
	if err != nil {
		return added, false, err
	}
	changed, err := WriteIndex(l, all, now)
	return added, changed, err
}

// aliasHomes returns the homes that alias-scoped citations name and that are
// not already being credited, so `[[fixit-team/LEDGER#^m12]]` counts even
// when the cwd resolves to no home at all.
func (e Env) aliasHomes(c Citations, already []Home) []Home {
	if len(c.Scoped) == 0 {
		return nil
	}
	all, d, err := e.Discover()
	if d != nil || err != nil {
		return nil
	}
	seen := map[string]bool{}
	for _, h := range already {
		seen[h.Path] = true
	}
	var out []Home
	for _, h := range all {
		if c.Scoped[h.Alias] != nil && !seen[h.Path] {
			seen[h.Path] = true
			out = append(out, h)
		}
	}
	return out
}

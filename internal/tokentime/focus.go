package tokentime

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/claudeguards"
	"github.com/henderson-tech/vybava/internal/shellseg"
	"github.com/henderson-tech/vybava/internal/transcripts"
)

// Focus: the repository an agent is working in. Agents reach sibling
// repositories by absolute path — an Edit of /work/b/x.go, a
// `(cd /work/b && git commit …)`, a Codex workdir — while every record keeps
// the cwd the session started in, so an AI minute filed by cwd lands in the
// wrong project. Each transcript keeps a focus root instead: the cwd's root
// until the agent WRITES somewhere else, back to it at a person's prompt or a
// new cwd. Reads never move it. Human minutes, tokens and the rollup stay on
// the cwd.

// focusRule names the attribution rule a store's AI beats follow; meta
// focus_rule holds it once the re-attribution of older history is settled.
const focusRule = "1"

// focusState is a transcript's focus, carried in its parse state across
// sweeps and passes.
type focusState struct {
	// Base is the root of the record cwd; Root the focus — Base until a write
	// lands elsewhere.
	Base string `json:"base,omitempty"`
	Root string `json:"root,omitempty"`
	// Touched are the roots the current response's tool calls wrote into.
	Touched []string `json:"touched,omitempty"`
	// Msg is the Claude message Touched belongs to. Its tool calls arrive in
	// later records under the same id, so its minute is recorded only when
	// the next message or prompt starts, or the transcript settles.
	Msg *claudeMsg `json:"msg,omitempty"`
}

type claudeMsg struct {
	ID string `json:"id"`
	At int64  `json:"at"` // unix seconds of its first record
	// Beat: the message owes a minute — a token read charged it, or a backlog read it.
	Beat bool `json:"beat,omitempty"`
	// Copy: a token read found it seen while history is re-read; it is
	// staged and found like its original (see copyFound).
	Copy bool `json:"copy,omitempty"`
}

// rebase points the focus back at a cwd's root: a person's prompt, or a cwd in another repository.
func (f *focusState) rebase(base string) { f.Base, f.Root, f.Touched = base, base, nil }

func (f *focusState) touch(root string) {
	f.Root = root
	if !slices.Contains(f.Touched, root) {
		f.Touched = append(f.Touched, root)
	}
}

// roots are where a response's minute goes: every root it wrote into, else the focus.
func (f *focusState) roots() []string {
	if len(f.Touched) > 0 {
		return f.Touched
	}
	return []string{f.Root}
}

// wrote is where one tool call wrote: a file, or a directory a command ran in.
type wrote struct {
	path string
	dir  bool
}

// claudeWrites lists where a Claude tool call wrote: the file an
// Edit/Write/MultiEdit/NotebookEdit changed, the directories a Bash
// command's writing segments ran in once a cd, git -C or --cwd moved them off
// cwd, and an MCP tool's `cwd` (a read-only command there writes nowhere).
// Read, Glob, Grep and WebFetch write nowhere.
func claudeWrites(use transcripts.ToolUse, cwd string) []wrote {
	switch use.Name {
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		var in struct {
			FilePath     string `json:"file_path"`
			NotebookPath string `json:"notebook_path"`
		}
		if json.Unmarshal(use.Input, &in) != nil {
			return nil
		}
		if p := resolvePath(firstNonEmpty(in.FilePath, in.NotebookPath), cwd); p != "" {
			return []wrote{{path: p}}
		}
	case "Bash":
		var in struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(use.Input, &in) == nil {
			return commandDirs(in.Command, cwd, cwd)
		}
	default:
		if !strings.HasPrefix(use.Name, "mcp__") {
			return nil
		}
		var in struct {
			Cwd     string          `json:"cwd"`
			Argv    []string        `json:"argv"`
			Command json.RawMessage `json:"command"`
		}
		if json.Unmarshal(use.Input, &in) != nil || in.Cwd == "" {
			return nil
		}
		dir := resolvePath(in.Cwd, cwd)
		var cmd string
		if json.Unmarshal(in.Command, &cmd) != nil && len(in.Argv) == 0 {
			_ = json.Unmarshal(in.Command, &in.Argv) // an argv-shaped command
		}
		if cmd == "" && len(in.Argv) > 0 {
			cmd = transcripts.ArgvCommand(in.Argv)
		}
		if cmd != "" {
			return commandDirs(cmd, dir, cwd)
		}
		if dir != "" && dir != filepath.Clean(cwd) {
			return []wrote{{path: dir, dir: true}}
		}
	}
	return nil
}

// codexWrites lists where a Codex call wrote: the directories an
// exec_command's writing segments ran in (its workdir, or where a cd moved
// them) when that is not cwd, and the files an apply_patch names.
func codexWrites(it transcripts.ResponseItem, cwd string) []wrote {
	cmds, patches := it.Commands()
	var out []wrote
	for _, c := range cmds {
		base := cwd
		if c.Workdir != "" {
			base = resolvePath(c.Workdir, cwd)
		}
		out = append(out, commandDirs(c.Cmd, base, cwd)...)
	}
	for _, p := range patches {
		for _, file := range transcripts.PatchFiles(p) {
			if path := resolvePath(file, cwd); path != "" {
				out = append(out, wrote{path: path})
			}
		}
	}
	return out
}

// commandDirs lists where cmd's segments, starting in base, wrote: the file
// each output redirect names, and the directory a writing segment runs in
// when that is not cwd or a cd, git -C or --cwd put it there — `cd <cwd> &&
// git commit` brings the focus home, a bare command in cwd says nothing. A
// segment that only inspects writes just its redirects: `cd lib && git diff >
// /tmp/x` writes in no repository. Where a segment runs is claudeguards'
// RunDirs, every literal cd taken to have applied; a `--cwd <dir>` moves that
// segment alone. A segment whose directory is unproven runs in base.
func commandDirs(cmd, base, cwd string) []wrote {
	if base == "" {
		return nil
	}
	var out []wrote
	add := func(w wrote) {
		if !slices.Contains(out, w) {
			out = append(out, w)
		}
	}
	for _, seg := range claudeguards.RunDirs(cmd, base) {
		dir, moved := filepath.Clean(seg.Dir), seg.Moved
		if d, ok := cwdFlag(seg.Text, dir); ok {
			dir, moved = filepath.Clean(d), true
		}
		writes := !readOnly(seg.Text)
		for _, target := range redirectTargets(seg.Text) {
			if p := resolvePath(target, dir); p != "" && !strings.ContainsAny(target, "$`*?[{") {
				add(wrote{path: p})
			} else {
				writes = true // a target not spelled out: somewhere around here
			}
		}
		if writes && (moved || dir != filepath.Clean(cwd)) {
			add(wrote{path: dir, dir: true})
		}
	}
	return out
}

// readOnlyWords inspect and print; they never change the tree they run in
// (an output redirect writes the file it names: see commandDirs). cd and
// pushd/popd only move.
var readOnlyWords = map[string]bool{
	"cd": true, "ls": true, "cat": true, "head": true, "tail": true, "less": true, "rg": true, "grep": true,
	"find": true, "fd": true, "wc": true, "jq": true, "stat": true, "file": true, "tree": true, "du": true,
	"pwd": true, "which": true, "echo": true, "printf": true, "true": true, "awk": true,
}

// readOnlyGit are the git subcommands that only inspect.
var readOnlyGit = map[string]bool{
	"log": true, "show": true, "status": true, "diff": true, "blame": true, "rev-parse": true,
	"ls-files": true, "grep": true, "fetch": true,
}

// branchChanges are the `git branch` flags that delete, move, copy or
// re-point a branch.
var branchChanges = map[string]bool{
	"-d": true, "-D": true, "-m": true, "-M": true, "-c": true, "-C": true, "-f": true, "-u": true,
	"--delete": true, "--move": true, "--copy": true, "--force": true, "--unset-upstream": true, "--edit-description": true,
}

// readOnly reports whether one segment's command only inspects; its output
// redirects are judged apart.
func readOnly(seg string) bool {
	fields := shellseg.Fields(shellseg.TrimAssignments(seg))
	switch word := shellseg.CommandWord(seg); {
	case readOnlyWords[word]:
		return true
	case word == "sed":
		return slices.Contains(fields, "-n") && !slices.ContainsFunc(fields, func(f string) bool {
			return strings.HasPrefix(f, "-i") || strings.HasPrefix(f, "--in-place")
		})
	case word == "git":
		return gitReadOnly(fields)
	}
	return false
}

// gitReadOnly: an inspecting subcommand, `branch` listing (`--list`, or no
// operand and no changing flag), `remote` showing, `config --get`.
func gitReadOnly(fields []string) bool {
	i := 1
	for ; i < len(fields) && strings.HasPrefix(fields[i], "-"); i++ {
		if name, _, joined := strings.Cut(fields[i], "="); !joined && (name == "-C" || name == "-c" || name == "--git-dir" || name == "--work-tree" || name == "--namespace") {
			i++
		}
	}
	if i >= len(fields) {
		return true // bare git, or only options: prints help
	}
	sub, args := fields[i], fields[i+1:]
	switch sub {
	case "branch":
		if slices.Contains(args, "--list") || slices.Contains(args, "-l") {
			return true
		}
		for _, a := range args {
			if !strings.HasPrefix(a, "-") || branchChanges[a] || strings.HasPrefix(a, "--set-upstream-to") {
				return false // a branch name creates one; these flags change one
			}
		}
		return true
	case "remote":
		return len(args) == 0 || args[0] == "-v" || args[0] == "--verbose" || args[0] == "show" || args[0] == "get-url"
	case "config":
		return slices.ContainsFunc(args, func(a string) bool {
			return a == "--get" || a == "--get-all" || a == "--get-regexp" || a == "--list" || a == "-l"
		})
	}
	return readOnlyGit[sub]
}

// redirectTargets lists the files a segment's unquoted output redirects
// write — `cat > f`, `echo x >> f`, `make &>log` — the one way a read-only
// word writes; a target that is not a plain word comes back as spelled, or
// "". A redirect to /dev/null or onto another descriptor (2>&1) writes
// nothing.
func redirectTargets(seg string) []string {
	var out []string
	var quote byte
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else if c == '\\' && quote == '"' {
				i++
			}
		case c == '\\':
			i++
		case c == '\'' || c == '"':
			quote = c
		case c == '>':
			j := i + 1
			for j < len(seg) && (seg[j] == '>' || seg[j] == '|') {
				j++
			}
			if j < len(seg) && seg[j] == '&' {
				i = j // onto a descriptor
				continue
			}
			for j < len(seg) && (seg[j] == ' ' || seg[j] == '\t') {
				j++
			}
			end := j
			for end < len(seg) && !strings.ContainsRune(" \t;&|<>()", rune(seg[end])) {
				end++
			}
			if target := strings.Trim(seg[j:end], `"'`); target != "/dev/null" {
				out = append(out, target)
			}
			i = end - 1
		}
	}
	return out
}

// cwdFlag is the directory a segment's `--cwd <dir>` / `--cwd=<dir>` names,
// resolved against dir; a variable or glob is never guessed.
func cwdFlag(seg, dir string) (string, bool) {
	fields := shellseg.Fields(seg)
	for i, f := range fields {
		value, ok := strings.CutPrefix(f, "--cwd=")
		if !ok && f == "--cwd" && i+1 < len(fields) {
			value, ok = fields[i+1], true
		}
		if ok && value != "" && !strings.ContainsAny(value, "$`*?[{") {
			return resolvePath(value, dir), true
		}
	}
	return "", false
}

// resolvePath makes p absolute against dir, expanding a leading ~; "" when
// a relative p has no dir to resolve against.
func resolvePath(p, dir string) string {
	switch {
	case p == "":
		return ""
	case p == "~" || strings.HasPrefix(p, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		return filepath.Join(home, strings.TrimPrefix(p[1:], "/"))
	case filepath.IsAbs(p):
		return filepath.Clean(p)
	case dir == "":
		return ""
	}
	return filepath.Join(dir, p)
}

type repoAnswer struct {
	root string
	ok   bool
}

// writeRoot is the repository a write landed in. ok is false outside every
// repository, inside the agents' own state (plans, memory, handoffs under
// ~/.claude, ~/.codex) and inside Options.SideDirs (deliverables, backups):
// none is a project's own work.
func (ix *indexer) writeRoot(w wrote) (string, bool) {
	key := w.path
	if !w.dir {
		key = filepath.Dir(w.path)
	}
	if a, ok := ix.repoMemo[key]; ok {
		return a.root, a.ok
	}
	var a repoAnswer
	if !slices.ContainsFunc(ix.sideDirs, func(d string) bool { return key == d || strings.HasPrefix(key, d+string(filepath.Separator)) }) {
		a.root, a.ok = transcripts.RepoRoot(key)
	}
	if a.ok {
		// A path an agent typed may differ from the disk in case only; the
		// record cwd never does.
		spelled, ok := ix.spelled[a.root]
		if !ok {
			spelled = transcripts.OnDiskCase(a.root)
			ix.spelled[a.root] = spelled
		}
		a.root = spelled
	}
	ix.repoMemo[key] = a
	return a.root, a.ok
}

// touch moves the focus to every repository writes landed in.
func (ix *indexer) touch(f *focusState, writes []wrote) {
	for _, w := range writes {
		if root, ok := ix.writeRoot(w); ok {
			f.touch(root)
		}
	}
}

// syncBase rebases the focus when the record cwd now lies in another
// repository (or the state predates focus); a pending Claude message is
// recorded first, under the focus it had.
func (ix *indexer) syncBase(f *focusState, cwd string, mode readMode) {
	if base := ix.root(cwd); base != f.Base || f.Root == "" && base != "" {
		ix.flushMsg(f, mode)
		f.rebase(base)
	}
}

// flushMsg records the pending Claude message's minute, if it owes one.
func (ix *indexer) flushMsg(f *focusState, mode readMode) {
	m := f.Msg
	if m == nil {
		return
	}
	switch {
	case m.Beat:
		ix.aiBeat(time.Unix(m.At, 0), f.roots(), identity("claude", m.ID), mode)
	case m.Copy && ix.staging:
		ix.copyFound(time.Unix(m.At, 0), f.roots(), identity("claude", m.ID))
	}
	f.Msg, f.Touched = nil, nil
}

// copyFound stages, while history is re-read, the minute of a response a
// token read found already seen, and marks it found. A copy adds no minute
// to beats, but an archived or copied transcript is still evidence for the
// day its vanished original answered in — the re-read stages copies alike,
// unable to tell them apart. Without it, a rollout archived before its
// original's re-read would leave every day it answered on cwd attribution.
func (ix *indexer) copyFound(ts time.Time, roots []string, key int64) {
	ix.aiBeat(ts, roots, key, readFocus)
}

// fileMinute is an AI minute of one file under one root: a file_beats row,
// or a staged one (focus_beats).
type fileMinute struct {
	minute int64
	file   int64
	root   string
}

// fileKey names a transcript or rollout in file_beats: its path's identity,
// kept after the file is deleted, like its minutes.
func fileKey(path string) int64 { return identity("file", path) }

type foundKey struct{ id, day int64 }

// aiBeat records a response's minute under roots, in beats and as the
// minute of the file being read (ix.source). While older history is being
// re-attributed (ix.staging) it also stages the minute and marks the
// response found on its day; the re-read itself (readFocus) only stages.
func (ix *indexer) aiBeat(ts time.Time, roots []string, key int64, mode readMode) {
	if ts.IsZero() {
		return
	}
	minute := ts.Unix() / 60
	for _, root := range roots {
		fm := fileMinute{minute: minute, file: ix.source, root: root}
		if mode != readFocus {
			ix.beats[beatKey{minute: minute, root: root, kind: beatAI}] = struct{}{}
			ix.fileBeats[fm] = struct{}{}
		}
		if ix.staging {
			ix.staged[fm] = struct{}{}
		}
	}
	if ix.staging {
		ix.found[foundKey{id: key, day: ts.Unix() / 86400}] = struct{}{}
	}
}

// whole reports a pending message begun staleTail ago or more: its tool
// calls cannot still be on their way.
func (ix *indexer) whole(m *claudeMsg) bool {
	return m != nil && ix.now.Unix()-m.At >= int64(staleTail/time.Second)
}

// settle records the last message of a Claude transcript nothing was
// appended to, once it is whole.
func (ix *indexer) settle(t target, row fileRow) {
	if t.codex || !strings.Contains(row.state, `"msg"`) {
		return
	}
	var cs codexState
	if json.Unmarshal([]byte(row.state), &cs) != nil || !ix.whole(cs.Focus.Msg) {
		return
	}
	ix.source = fileKey(t.path)
	ix.flushMsg(&cs.Focus, readTokens)
	raw, _ := json.Marshal(cs)
	row.state = string(raw)
	ix.files[t.path], ix.read[t.path] = row, row
}

// focusOf reads a stored focus debt: what of a file was read before its AI
// minutes followed the focus. A file read before focus existed (empty) owes
// everything up to its stored cursor; an unreadable debt owes it again.
func focusOf(stored string, cur transcripts.Cursor) beatsLag {
	var l beatsLag
	if stored == "" || json.Unmarshal([]byte(stored), &l) != nil {
		return beatsLag{Until: cur.Offset}
	}
	return l
}

// focusBacklog re-reads, while the re-attribution is pending, the history
// each file was read before the focus rule — beats only, into the staging
// tables, charging nothing — with whatever budget the reads before it left,
// newest files first. It reports whether every file on disk is re-read.
func (ix *indexer) focusBacklog(targets []target, known map[string]fileRow, budget int64, perCommit int) (bool, error) {
	if !ix.staging {
		return false, nil
	}
	order := append([]target(nil), targets...)
	sort.SliceStable(order, func(i, j int) bool { return order[i].info.ModTime().After(order[j].info.ModTime()) })
	open, sinceCommit, lastCommit := 0, 0, time.Now()
	for _, t := range order {
		row, ok := ix.read[t.path]
		if !ok {
			if row, ok = known[t.path]; !ok {
				continue // new and left unread by the budget: its first read stages its minutes
			}
		}
		if row.focus == beatsDone {
			continue
		}
		lag := focusOf(row.focus, known[t.path].cur)
		if !lag.done() && ix.ctx.Err() == nil && (budget <= 0 || ix.report.ReadBytes < budget) {
			var err error
			// A file that shrank under its debt took that history along: its
			// responses stay unfound, so their days keep the minutes they had.
			if lag, _, err = ix.catchUp(t, &row, lag, budget, readFocus); err != nil {
				return false, err
			}
		}
		if !lag.done() {
			if _, err := os.Lstat(t.path); errors.Is(err, os.ErrNotExist) {
				// Deleted since the pass listed it — a long re-read outlives
				// some transcripts: it took that history along, as a shrunk
				// one does, and owes nothing that could hold the swap back.
				lag.Until = lag.Cursor.Offset
			}
		}
		if !lag.done() {
			open++
			ix.report.BeatsPendingBytes += lag.Until - lag.Cursor.Offset
		}
		if focus := lag.encode(); focus != row.focus {
			row.focus = focus
			ix.read[t.path] = row
			ix.focusDebts[t.path] = focus
			sinceCommit++
		}
		if (sinceCommit > 0 || ix.dirty > 0) && (ix.dirty >= commitBytes || sinceCommit >= perCommit || time.Since(lastCommit) >= commitEvery) {
			if err := ix.flush(); err != nil {
				return false, err
			}
			sinceCommit, lastCommit = 0, time.Now()
		}
	}
	return open == 0 && ix.ctx.Err() == nil, nil
}

// pendingFound readies the settling commit for the Claude messages still
// waiting in a file's state for their tool calls — a live session always has
// one. A charged one is found: its minute is in no table yet, and is recorded
// where its calls point once it is whole, by then straight into beats, so
// swapping its day loses nothing. A copy is staged and found now; nothing
// records it later.
func (ix *indexer) pendingFound(targets []target, known map[string]fileRow) {
	for _, t := range targets {
		row, ok := ix.read[t.path]
		if !ok {
			row = known[t.path]
		}
		if t.codex || !strings.Contains(row.state, `"msg"`) {
			continue
		}
		var cs codexState
		if json.Unmarshal([]byte(row.state), &cs) != nil || cs.Focus.Msg == nil {
			continue
		}
		m := cs.Focus.Msg
		ix.source = fileKey(t.path)
		switch key := identity("claude", m.ID); {
		case m.Beat:
			ix.found[foundKey{id: key, day: m.At / 86400}] = struct{}{}
		case m.Copy:
			ix.copyFound(time.Unix(m.At, 0), cs.Focus.roots(), key)
		}
	}
}

// commitStaged writes the staged minutes and found responses into commit's
// transaction and, on the pass that re-read the last file, settles.
func (ix *indexer) commitStaged(tx *sql.Tx) error {
	if len(ix.staged) > 0 {
		stmt, err := tx.Prepare("INSERT OR IGNORE INTO focus_beats(minute, project, file) VALUES(?, ?, ?)")
		if err != nil {
			return err
		}
		defer stmt.Close()
		for k := range ix.staged {
			project, err := ix.id(tx, "projects", "root", k.root, ix.projects)
			if err == nil {
				_, err = stmt.Exec(k.minute, project, k.file)
			}
			if err != nil {
				return err
			}
		}
	}
	if len(ix.found) > 0 {
		stmt, err := tx.Prepare("INSERT OR IGNORE INTO focus_found(id, day) VALUES(?, ?)")
		if err != nil {
			return err
		}
		defer stmt.Close()
		for k := range ix.found {
			if _, err := stmt.Exec(k.id, k.day); err != nil {
				return err
			}
		}
	}
	if ix.settleFocus {
		return settleFocus(tx)
	}
	return nil
}

// settleFocus swaps the AI minutes of every day it can vouch for for the
// re-read ones. Beats keep no provenance, so a day is replaced only when
// every response charged on it (its seen identities, which are permanent) was
// found again, on that same day, by a read that staged its minute — then no
// deleted transcript holds a minute of that day the re-read lacks. Every
// other day keeps the minutes it had: beats never shrink with a transcript.
// Each file's own minutes (file_beats) swap with them, and a day kept drops
// the re-read's: its cwd minutes belong to no file's timeline, so `beats
// --bridge` bridges them per project.
func settleFocus(tx *sql.Tx) error {
	for _, q := range []string{
		`CREATE TEMP TABLE focus_safe AS SELECT DISTINCT day FROM seen EXCEPT
			SELECT DISTINCT s.day FROM seen s WHERE NOT EXISTS (SELECT 1 FROM focus_found f WHERE f.id = s.id AND f.day = s.day)`,
		`DELETE FROM beats WHERE kind = 1 AND minute / 1440 IN (SELECT day FROM focus_safe)`,
		`INSERT OR IGNORE INTO beats(minute, project, kind)
			SELECT minute, project, 1 FROM focus_beats WHERE minute / 1440 IN (SELECT day FROM focus_safe)`,
		`DELETE FROM file_beats WHERE minute / 1440 IN (SELECT day FROM focus_safe)`,
		`INSERT OR IGNORE INTO file_beats(minute, file, project)
			SELECT minute, file, project FROM focus_beats WHERE minute / 1440 IN (SELECT day FROM focus_safe)`,
		`DROP TABLE focus_safe`,
		`DELETE FROM focus_beats`,
		`DELETE FROM focus_found`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	return setMeta(tx, "focus_rule", focusRule)
}

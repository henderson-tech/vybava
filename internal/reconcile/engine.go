package reconcile

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Engine drives one box. Every mutating verb (Run, Force, Rollback) takes the
// shared lock; Status never writes anything — not state, not alert markers,
// not the checkout.
type Engine struct {
	M       Manifest
	Version string // the running vybava version, checked against the pin
	Out     io.Writer
	Err     io.Writer
	Now     func() time.Time
	// LockTimeout bounds the wait for the shared lock (default 30s).
	LockTimeout time.Duration
}

// Issue is one classified error line. Kinds: symlink, escape, permission,
// write, hook, config, git.
type Issue struct {
	Kind    string `json:"kind"`
	Path    string `json:"path,omitempty"`
	Message string `json:"message"`
}

// Result is what one tick (run / rollback / status sweep) produced.
type Result struct {
	Action        string   `json:"action"`
	Commit        string   `json:"commit"`
	CommitSubject string   `json:"commit_subject,omitempty"`
	Mode          string   `json:"mode"`
	Applied       []string `json:"applied"`
	Pending       []string `json:"pending"`
	Held          []string `json:"held"`
	SkippedApps   []string `json:"skipped_apps"`
	RollNotes     []string `json:"roll_manually"`
	FailedHooks   []string `json:"failed_hooks"`
	Errors        []Issue  `json:"errors"`
	LastGood      string   `json:"last_good,omitempty"`
	Pin           string   `json:"pin,omitempty"`
	Digest        string   `json:"digest,omitempty"`
}

// ExitError carries the process exit code for the CLI.
type ExitError struct {
	Code int
	Msg  string
}

func (e *ExitError) Error() string { return e.Msg }
func (e *ExitError) ExitCode() int { return e.Code }

func (e *Engine) out() io.Writer {
	if e.Out == nil {
		return io.Discard
	}
	return e.Out
}

func (e *Engine) errw() io.Writer {
	if e.Err == nil {
		return io.Discard
	}
	return e.Err
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) log(format string, args ...any) {
	fmt.Fprintf(e.out(), "[%s] %s\n", e.now().Format("2006-01-02T15:04:05-0700"), fmt.Sprintf(format, args...))
}

func (e *Engine) logErr(format string, args ...any) {
	fmt.Fprintf(e.errw(), "[%s] %s\n", e.now().Format("2006-01-02T15:04:05-0700"), fmt.Sprintf(format, args...))
}

func (e *Engine) state() State { return State{Dir: e.M.StateDir} }
func (e *Engine) git() gitRepo { return gitRepo{dir: e.M.Clone} }
func (e *Engine) lockTimeout() time.Duration {
	if e.LockTimeout > 0 {
		return e.LockTimeout
	}
	return 30 * time.Second
}

// Mode reads the mode file: anything but "converge" is report.
func (e *Engine) Mode() string {
	if readTrim(e.M.ModeFile) == "converge" {
		return "converge"
	}
	return "report"
}

func (e *Engine) versionMismatch() string {
	pin := e.M.VybavaVersion
	if pin == "" || e.Version == "" || e.Version == "dev" {
		return ""
	}
	if strings.TrimPrefix(pin, "v") == strings.TrimPrefix(e.Version, "v") {
		return ""
	}
	return fmt.Sprintf("manifest pins vybava %s, running %s", pin, e.Version)
}

// ── run ──────────────────────────────────────────────────────────────────────

// Run is the cron entry point: pull, sweep, hooks, report, alert, record.
func (e *Engine) Run() (Result, error) {
	lock, err := AcquireLock(e.M.LockFile, e.lockTimeout())
	if err != nil {
		return Result{}, &ExitError{Code: 1, Msg: err.Error()}
	}
	defer lock.Release()
	st := e.state()
	if err := st.Ensure(); err != nil {
		return Result{}, err
	}
	g := e.git()
	if err := g.fetchMain(); err != nil {
		return Result{}, err
	}
	target := "origin/main"
	if pin := st.Pin(); pin != "" {
		target = pin
		e.log("pinned to %s by rollback — run `vybava reconcile rollback --unpin` to follow origin/main again", short(pin))
	}
	head, err := g.revParse("HEAD")
	if err != nil {
		return Result{}, err
	}
	want, err := g.revParse(target)
	if err != nil {
		return Result{}, err
	}
	if head != want {
		e.log("pull: %s -> %s", short(head), short(want))
		if err := g.resetHard(target); err != nil {
			return Result{}, err
		}
	}
	return e.tick("run", e.Mode(), false)
}

// Rollback re-converges the box to the last-good commit (or the given one)
// and pins the clone there until --unpin. HELD files stay HELD.
func (e *Engine) Rollback(sha string, unpin bool) (Result, error) {
	lock, err := AcquireLock(e.M.LockFile, e.lockTimeout())
	if err != nil {
		return Result{}, &ExitError{Code: 1, Msg: err.Error()}
	}
	defer lock.Release()
	st := e.state()
	if err := st.Ensure(); err != nil {
		return Result{}, err
	}
	if unpin {
		if err := st.SetPin(""); err != nil {
			return Result{}, err
		}
		e.log("rollback: pin cleared — the next tick follows origin/main")
		return Result{Action: "rollback"}, nil
	}
	if sha == "" {
		sha = st.LastGood()
		if sha == "" {
			return Result{}, &ExitError{Code: 1, Msg: "no last-good commit recorded yet and no <sha> given"}
		}
	}
	g := e.git()
	full, err := g.revParse(sha + "^{commit}")
	if err != nil {
		return Result{}, &ExitError{Code: 1, Msg: fmt.Sprintf("unknown commit %s (fetch first?): %v", sha, err)}
	}
	head, _ := g.revParse("HEAD")
	if head != full {
		e.log("rollback: %s -> %s", short(head), short(full))
		if err := g.resetHard(full); err != nil {
			return Result{}, err
		}
	}
	if err := st.SetPin(full); err != nil {
		return Result{}, err
	}
	e.log("rollback: pinned to %s (%s) — converging", short(full), g.subject(full))
	return e.tick("rollback", "converge", false)
}

// Status is the read-only sweep of the checkout as it stands.
func (e *Engine) Status() (Result, error) {
	return e.tick("status", "report", true)
}

// ── the tick ─────────────────────────────────────────────────────────────────

type nginxRB struct {
	rp, dest, snap string
}

type sweep struct {
	e         *Engine
	mode      string
	status    bool
	st        State
	hooks     map[string]bool
	preloaded map[string]bool
	rbDir     string
	nginxRB   []nginxRB
	copyFail  bool
	res       *Result
}

func (e *Engine) tick(action, mode string, statusOnly bool) (Result, error) {
	g := e.git()
	head, err := g.revParse("HEAD")
	if err != nil {
		return Result{}, err
	}
	res := Result{Action: action, Commit: head, CommitSubject: g.subject(head), Mode: mode,
		Applied: []string{}, Pending: []string{}, Held: []string{}, SkippedApps: []string{},
		RollNotes: []string{}, FailedHooks: []string{}, Errors: []Issue{}}
	if statusOnly {
		res.Mode = e.Mode()
	}
	st := e.state()
	if !statusOnly {
		res.LastGood = st.LastGood()
		res.Pin = st.Pin()
	}
	if mm := e.versionMismatch(); mm != "" {
		e.logErr("version: %s", mm)
	}

	sw := &sweep{e: e, mode: mode, status: statusOnly, st: st, hooks: map[string]bool{}, preloaded: map[string]bool{}, res: &res}
	if !statusOnly {
		// a hook that failed on an earlier tick retries now, even with no new
		// drift — applied.tsv already matches, so nothing else reschedules it
		for _, h := range st.PendingHooks() {
			sw.hooks[h] = true
			sw.preloaded[h] = true
		}
	}
	files, err := g.lsFiles()
	if err != nil {
		return res, err
	}
	for _, rp := range files {
		sw.file(rp)
	}
	if mode == "converge" {
		sw.runHooks()
	}
	sw.report()
	if statusOnly {
		return res, nil
	}
	e.alert(&res)
	ok := len(res.Errors) == 0 && len(res.FailedHooks) == 0
	if mode == "converge" && ok {
		if err := st.SetLastGood(head); err != nil {
			return res, err
		}
		res.LastGood = head
	}
	entry := HistoryEntry{Time: e.now(), Action: action, Commit: head, Mode: mode, OK: ok,
		Applied: res.Applied, Pending: res.Pending, Held: res.Held, Errors: res.Errors,
		RollNotes: res.RollNotes, SkippedApps: res.SkippedApps, FailedHooks: res.FailedHooks,
		LastGood: res.LastGood, Pin: res.Pin}
	if err := st.AppendHistory(entry); err != nil {
		return res, err
	}
	if err := e.writeMetrics(res); err != nil {
		e.logErr("metrics: %v", err)
	}
	if !ok {
		return res, &ExitError{Code: 1, Msg: fmt.Sprintf("%s finished with %d error(s)", action, len(res.Errors)+len(res.FailedHooks))}
	}
	return res, nil
}

func (s *sweep) errorf(kind, rp, format string, args ...any) {
	s.res.Errors = append(s.res.Errors, Issue{Kind: kind, Path: rp, Message: fmt.Sprintf(format, args...)})
}

// contain applies the compose containment (and the canonical-destination
// rule shared by every hook). ok=false means the file was handled.
func (s *sweep) contain(rp string, t Target) (skipApp bool, ok bool) {
	e := s.e
	if isSymlink(filepath.Join(e.M.Clone, rp)) {
		s.errorf("symlink", rp, "%s is a symlink — mapped sources must be regular files", rp)
		return false, false
	}
	canon, err := canonical(t.Dest)
	if err != nil {
		s.errorf("write", rp, "%s -> %s: cannot resolve destination: %v", rp, t.Dest, err)
		return false, false
	}
	if canon != t.Dest {
		s.errorf("symlink", rp, "%s -> %s resolves elsewhere (symlinked component) — refused", rp, t.Dest)
		return false, false
	}
	if t.Hook == HookCompose && t.RequireLiveDir {
		appdir := filepath.Join(e.M.AppsRoot, t.App)
		if !isDir(appdir) || isSymlink(appdir) {
			return true, false
		}
		if !containedIn(canon, appdir) {
			s.errorf("escape", rp, "%s -> %s escapes %s (symlinked path) — refused", rp, t.Dest, appdir)
			return false, false
		}
	}
	return false, true
}

func hookKey(t Target) string {
	switch t.Hook {
	case HookNginx:
		return "nginx"
	case HookCompose:
		return "compose:" + t.App
	}
	return ""
}

func (s *sweep) file(rp string) {
	e := s.e
	t, ok := e.M.MapPath(rp)
	if !ok {
		return
	}
	skipApp, ok := s.contain(rp, t)
	if skipApp {
		s.res.SkippedApps = appendUnique(s.res.SkippedApps, t.App)
		return
	}
	if !ok {
		return
	}
	src := filepath.Join(e.M.Clone, rp)
	repoSHA := fileSHA(src)
	liveSHA := fileSHA(t.Dest)

	apply := func(label string) {
		if t.Hook == HookNginx && !s.snapshotNginx(rp, t.Dest) {
			return // no snapshot, no overwrite: the transaction rolls back without it
		}
		if err := applyFile(src, t.Dest); err != nil {
			s.res.Errors = append(s.res.Errors, classifyWriteError(rp, t.Dest, t.Owner, err))
			if t.Hook == HookNginx {
				s.copyFail = true
			}
			return
		}
		if err := s.st.RecordApplied(rp, repoSHA); err != nil {
			s.errorf("write", rp, "record applied %s: %v", rp, err)
			return
		}
		s.res.Applied = append(s.res.Applied, label)
		if k := hookKey(t); k != "" {
			s.hooks[k] = true
		}
	}

	if liveSHA == "" {
		// new file — installing is the desired git-driven flow
		if s.mode == "converge" {
			apply(rp + " (new)")
		} else {
			s.res.Pending = append(s.res.Pending, rp+" (new file)")
		}
		return
	}
	if liveSHA == repoSHA {
		if !s.status && s.st.AppliedSHA(rp) != repoSHA {
			if err := s.st.RecordApplied(rp, repoSHA); err != nil { // adopt in-sync
				s.errorf("write", rp, "record applied %s: %v", rp, err)
			}
		}
		return
	}
	if s.st.AppliedSHA(rp) == liveSHA {
		// live is exactly what we last applied — the repo moved: safe to converge
		if s.mode == "converge" {
			apply(rp)
		} else {
			s.res.Pending = append(s.res.Pending, rp)
		}
		return
	}
	// hand-edited live file (hotfix) — NEVER overwritten; backport or `force`
	s.res.Held = append(s.res.Held, rp)
}

// snapshotNginx must run BEFORE overwriting dest: nginx files converge as a
// transaction and are restored (with their applied record) if the shared
// nginx -t fails, so a bad conf never stays live-and-adopted. false = the
// snapshot failed and dest must not be touched.
func (s *sweep) snapshotNginx(rp, dest string) bool {
	if s.rbDir == "" {
		dir, err := os.MkdirTemp(s.st.Dir, "rollback.")
		if err != nil {
			s.errorf("write", rp, "cannot create rollback dir: %v", err)
			s.copyFail = true
			return false
		}
		s.rbDir = dir
	}
	snap := ""
	if isRegular(dest) {
		f, err := os.CreateTemp(s.rbDir, "snap.")
		if err != nil {
			s.errorf("write", rp, "cannot snapshot %s: %v", dest, err)
			s.copyFail = true
			return false
		}
		snap = f.Name()
		f.Close()
		if err := copyPreserve(dest, snap); err != nil {
			s.errorf("write", rp, "cannot snapshot %s: %v", dest, err)
			s.copyFail = true
			return false
		}
	}
	s.nginxRB = append(s.nginxRB, nginxRB{rp: rp, dest: dest, snap: snap})
	return true
}

// rollbackNginx restores every snapshotted nginx file; returns the count.
func (s *sweep) rollbackNginx() int {
	rolled := 0
	for _, rb := range s.nginxRB {
		if rb.snap != "" && isRegular(rb.snap) {
			if err := copyPreserve(rb.snap, rb.dest); err != nil {
				s.errorf("write", rb.rp, "rollback of %s failed: %v", rb.dest, err)
				continue
			}
			_ = s.st.RecordApplied(rb.rp, fileSHA(rb.dest))
		} else {
			_ = os.Remove(rb.dest)
			_ = s.st.UnrecordApplied(rb.rp)
		}
		rolled++
	}
	return rolled
}

func (s *sweep) runHooks() {
	e := s.e
	keys := make([]string, 0, len(s.hooks))
	for k := range s.hooks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, h := range keys {
		switch {
		case h == "nginx":
			if s.copyFail {
				rolled := s.rollbackNginx()
				s.errorf("hook", "", "nginx converge incomplete (copy failure) — rolled back %d conf file(s); nothing reloaded", rolled)
				if rolled == 0 || s.preloaded[h] {
					s.res.FailedHooks = append(s.res.FailedHooks, h)
				}
			} else if err := e.nginxTest(); err == nil {
				if err := e.nginxReload(); err == nil {
					e.log("hook: nginx tested + reloaded")
				} else {
					s.errorf("hook", "", "nginx reload failed after passing test: %v", err)
					s.res.FailedHooks = append(s.res.FailedHooks, h)
				}
			} else {
				// last-good rollback: restore every nginx file this run touched
				// and re-point applied.tsv at the restored content, so the next
				// tick retries the converge instead of adopting a bad conf.
				rolled := s.rollbackNginx()
				s.errorf("hook", "", "nginx -t FAILED — rolled back %d conf file(s) to the previous live state; fix in git", rolled)
				// a hook resumed from pending-hooks stays queued regardless of
				// the rollback: the already-live config still owes its reload.
				if rolled == 0 || s.preloaded[h] {
					s.res.FailedHooks = append(s.res.FailedHooks, h)
				}
			}
		case strings.HasPrefix(h, "compose:"):
			app := strings.TrimPrefix(h, "compose:")
			if contains(e.M.AutoRollApps, app) {
				if err := e.composeUp(app); err != nil {
					s.errorf("hook", "", "auto-roll of %s failed: %v", app, err)
					s.res.FailedHooks = append(s.res.FailedHooks, h)
				} else {
					e.log("hook: rolled %s (opt-in auto-roll)", app)
				}
			} else {
				s.res.RollNotes = append(s.res.RollNotes, app)
			}
		}
	}
	if err := s.st.WritePendingHooks(s.res.FailedHooks); err != nil {
		s.errorf("write", "", "pending-hooks: %v", err)
	}
	if s.rbDir != "" {
		_ = os.RemoveAll(s.rbDir)
	}
}

func (s *sweep) report() {
	e, r := s.e, s.res
	if len(r.Applied) > 0 {
		e.log("applied (%d): %s", len(r.Applied), joinSemi(r.Applied))
	}
	if len(r.Pending) > 0 {
		e.log("pending converge (%d, mode=%s): %s", len(r.Pending), r.Mode, joinSemi(r.Pending))
	}
	if len(r.Held) > 0 {
		e.log("HELD (hand-edited live, backport or force) (%d): %s", len(r.Held), joinSemi(r.Held))
	}
	if len(r.RollNotes) > 0 {
		e.log("compose converged, ROLL MANUALLY: %s ", strings.Join(r.RollNotes, " "))
	}
	if len(r.SkippedApps) > 0 {
		apps := append([]string(nil), r.SkippedApps...)
		sort.Strings(apps)
		e.log("skipped repo apps with no live dir: %s ", strings.Join(apps, " "))
	}
	if len(r.Errors) > 0 {
		e.logErr("ERRORS: %s", joinSemi(issueMessages(r.Errors)))
	}
	r.Digest = e.digest(r)
}

func (e *Engine) digest(r *Result) string {
	var b strings.Builder
	if len(r.Held) > 0 {
		b.WriteString("HELD (hotfixed on VPS — backport to git or `vybava reconcile force <path>`):\n")
		for _, p := range r.Held {
			b.WriteString("  " + p + "\n")
		}
	}
	if len(r.RollNotes) > 0 {
		b.WriteString("Compose files converged — roll manually:\n")
		for _, a := range r.RollNotes {
			b.WriteString("  cd " + filepath.Join(e.M.AppsRoot, a) + " && docker compose up -d\n")
		}
	}
	if len(r.Errors) > 0 {
		b.WriteString("Errors:\n")
		for _, i := range r.Errors {
			b.WriteString("  " + i.Message + "\n")
		}
	}
	if r.Mode == "report" && len(r.Pending) > 0 {
		b.WriteString("Pending (report mode — flip " + e.M.ModeFile + " to 'converge' to apply):\n")
		for _, p := range r.Pending {
			b.WriteString("  " + p + "\n")
		}
	}
	return b.String()
}

// ── force ────────────────────────────────────────────────────────────────────

// Force stamps the repo version of ONE file over the live one, backing the
// live copy up first, and runs the file's hook under the same contract as a
// converge (a failing nginx test restores the previous state).
func (e *Engine) Force(rp string) error {
	lock, err := AcquireLock(e.M.LockFile, e.lockTimeout())
	if err != nil {
		return &ExitError{Code: 1, Msg: err.Error()}
	}
	defer lock.Release()
	st := e.state()
	if err := st.Ensure(); err != nil {
		return err
	}
	fail := func(format string, args ...any) error {
		return &ExitError{Code: 1, Msg: fmt.Sprintf(format, args...)}
	}
	// caller-controlled: reject absolute paths and any ".." segment, and
	// require a file git actually tracks — mappings preserve the suffix, so a
	// traversal would escape the mapped destination root.
	if rp == "" {
		return fail("usage: reconcile force <repo-relative-path>")
	}
	if filepath.IsAbs(rp) || strings.HasPrefix(rp, "/") {
		return fail("absolute path not allowed: %s", rp)
	}
	for _, seg := range strings.Split(rp, "/") {
		if seg == ".." {
			return fail("path traversal not allowed: %s", rp)
		}
	}
	g := e.git()
	if !g.tracks(rp) {
		return fail("not a git-tracked repo file: %s", rp)
	}
	src := filepath.Join(e.M.Clone, rp)
	if isSymlink(src) {
		return fail("refusing symlinked source (root copy would dereference): %s", rp)
	}
	t, ok := e.M.MapPath(rp)
	if !ok {
		return fail("not a mapped path: %s", rp)
	}
	canon, err := canonical(t.Dest)
	if err != nil {
		return fail("cannot resolve %s: %v", t.Dest, err)
	}
	if canon != t.Dest {
		return fail("destination resolves elsewhere (symlinked component) — refusing %s", t.Dest)
	}
	if t.Hook == HookCompose && t.RequireLiveDir {
		// same containment as the sweep: never materialize a new app tree, and
		// never follow a symlinked app dir out of apps_root
		appdir := filepath.Join(e.M.AppsRoot, t.App)
		if !isDir(appdir) || isSymlink(appdir) {
			return fail("app dir absent or symlinked — refusing %s", appdir)
		}
		if !containedIn(canon, appdir) {
			return fail("destination escapes %s — refusing", appdir)
		}
	}
	bak := ""
	if isRegular(t.Dest) {
		// the hand-edited live version may be the only copy of a hotfix — keep
		// it; a random suffix so two forces in one second never collide.
		if err := os.MkdirAll(st.Backups(), 0o755); err != nil {
			return err
		}
		f, err := os.CreateTemp(st.Backups(), e.now().Format("20060102T150405")+"-"+filepath.Base(t.Dest)+".*")
		if err != nil {
			return err
		}
		bak = f.Name()
		f.Close()
		content, err := os.ReadFile(t.Dest)
		if err != nil {
			return err
		}
		if err := atomicWrite(bak, content, 0o600); err != nil {
			return err
		}
		e.log("force: backed up live %s -> %s", t.Dest, bak)
	}
	if err := applyFile(src, t.Dest); err != nil {
		issue := classifyWriteError(rp, t.Dest, t.Owner, err)
		return fail("force: %s", issue.Message)
	}
	repoSHA := fileSHA(src)
	record := func() error { return st.RecordApplied(rp, repoSHA) }
	history := func(ok bool, issues ...Issue) {
		_ = st.AppendHistory(HistoryEntry{Time: e.now(), Action: "force", Path: rp, Mode: "converge", OK: ok, Errors: issues, Applied: []string{rp}})
	}
	switch t.Hook {
	case HookNginx:
		if err := e.nginxTest(); err != nil {
			if bak != "" {
				content, rerr := os.ReadFile(bak)
				if rerr == nil {
					rerr = writeLive(t.Dest, content, 0o644)
				}
				if rerr != nil {
					return fail("force: nginx -t FAILED and restoring %s from %s failed too: %v", t.Dest, bak, rerr)
				}
			} else {
				_ = os.Remove(t.Dest)
			}
			e.logErr("force: nginx -t FAILED — restored previous live state, nothing recorded; fix in git")
			history(false, Issue{Kind: "hook", Path: rp, Message: "nginx -t failed: " + err.Error()})
			return fail("force: nginx -t failed for %s — previous live state restored", rp)
		}
		if err := e.nginxReload(); err != nil {
			// config is valid, only the reload is outstanding: record the file
			// and persist the hook so the next tick retries the reload instead
			// of silently adopting the file hook-less.
			if rerr := record(); rerr != nil {
				return rerr
			}
			if qerr := st.QueueHook("nginx"); qerr != nil {
				return qerr
			}
			e.logErr("force: nginx reload failed after passing test — reload queued for retry")
			history(false, Issue{Kind: "hook", Path: rp, Message: "nginx reload failed: " + err.Error()})
			return fail("force: nginx reload failed for %s — queued for retry", rp)
		}
		e.log("force: nginx tested + reloaded")
	case HookCompose:
		e.log("force: compose file applied — roll manually: cd %s && docker compose up -d", filepath.Join(e.M.AppsRoot, t.App))
	}
	if err := record(); err != nil {
		return err
	}
	history(true)
	e.log("force: applied %s -> %s", rp, t.Dest)
	return nil
}

// ── hooks ────────────────────────────────────────────────────────────────────

func (e *Engine) runCmd(dir string, argv []string) error {
	if len(argv) == 0 {
		return errors.New("hook not configured in the manifest")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	var errb strings.Builder
	cmd.Stdout = io.Discard
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			return err
		}
		return fmt.Errorf("%w: %s", err, msg)
	}
	return nil
}

func (e *Engine) nginxTest() error {
	return e.runCmd(e.M.Hooks.Nginx.Workdir, e.M.Hooks.Nginx.Test)
}

func (e *Engine) nginxReload() error {
	return e.runCmd(e.M.Hooks.Nginx.Workdir, e.M.Hooks.Nginx.Reload)
}

func (e *Engine) composeUp(app string) error {
	return e.runCmd(filepath.Join(e.M.AppsRoot, app), e.M.Hooks.Compose)
}

// ── helpers ──────────────────────────────────────────────────────────────────

func joinSemi(items []string) string {
	var b strings.Builder
	for _, s := range items {
		b.WriteString(s + "; ")
	}
	return b.String()
}

func issueMessages(issues []Issue) []string {
	out := make([]string, len(issues))
	for i, is := range issues {
		out[i] = is.Message
	}
	return out
}

func appendUnique(list []string, s string) []string {
	if contains(list, s) {
		return list
	}
	return append(list, s)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

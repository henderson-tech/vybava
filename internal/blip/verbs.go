package blip

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/runx"
)

// Tool binds the verbs to a state dir and the argv prefix `up` re-executes
// (["blip"] for the applet, ["vybava","blip"] through the hub).
type Tool struct {
	Dir  string
	Self []string // argv[1:] prefix in front of "serve" (empty for the applet)
}

// Open resolves the state dir.
func Open(self []string) (*Tool, error) {
	dir, err := StateDir()
	if err != nil {
		return nil, err
	}
	return &Tool{Dir: dir, Self: self}, nil
}

func (t *Tool) paths(name string) Paths { return pathsFor(t.Dir, name) }

// Serve is the hidden daemon entry `up` re-executes.
func (t *Tool) Serve(name, listen, to string) error {
	if err := validName(name); err != nil {
		return err
	}
	mode, upstream, err := ParseUpstream(to)
	if err != nil {
		return err
	}
	return Serve(Config{Name: name, Mode: mode, Listen: listen, Upstream: upstream}, t.paths(name))
}

// connect finds a running proxy: NOT_RUNNING when no state file, STALE_STATE
// when the state file's daemon does not answer.
func (t *Tool) connect(name string) (Paths, *State, *client, error) {
	if err := validName(name); err != nil {
		return Paths{}, nil, nil, err
	}
	p := t.paths(name)
	st, err := readState(p.State)
	if errors.Is(err, os.ErrNotExist) {
		return p, nil, nil, diag(DiagNotRunning, "no proxy named "+name+" is up", "blip up "+name+" --listen <addr> --to <upstream>")
	}
	if err != nil {
		return p, nil, nil, diag(DiagStale, "state file unreadable: "+err.Error(), "blip down "+name)
	}
	c := dial(p.Sock)
	if _, err := c.status(); err != nil {
		return p, st, nil, diag(DiagStale, fmt.Sprintf("%s (pid %d) does not answer on its control socket", name, st.PID), "blip down "+name)
	}
	return p, st, c, nil
}

// URL is the address the app should point at.
func listenURL(mode, listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "localhost"
	}
	if mode == "http" {
		return "http://" + net.JoinHostPort(host, port)
	}
	return net.JoinHostPort(host, port)
}

func envHint(mode, listen string) string {
	if mode == "http" {
		return "EXPO_PUBLIC_API_URL=" + listenURL(mode, listen)
	}
	return "DATABASE_URL host:port → " + listenURL(mode, listen)
}

type upData struct {
	Config
	URL     string `json:"url"`
	EnvHint string `json:"env_hint"`
	PID     int    `json:"pid"`
	State   string `json:"state_file"`
	Log     string `json:"log"`
}

// Up starts (or finds) the named proxy and waits until it answers.
func (t *Tool) Up(name, listen, to string) (Result, error) {
	if err := validName(name); err != nil {
		return Result{}, err
	}
	mode, upstream, err := ParseUpstream(to)
	if err != nil {
		return Result{}, err
	}
	if _, _, err := net.SplitHostPort(listen); err != nil {
		return Result{}, diag(DiagListenInvalid, "--listen must be host:port or :port, got "+fmt.Sprintf("%q", listen), "blip up "+name+" --listen :9091 --to "+to)
	}
	p := t.paths(name)
	cfg := Config{Name: name, Mode: mode, Listen: listen, Upstream: upstream}
	next := []string{"blip " + name + " set delay 800ms", "blip " + name + " status"}
	if st, err := readState(p.State); err == nil {
		if live, err := dial(p.Sock).status(); err == nil {
			data := upData{Config: live.Config, URL: listenURL(live.Mode, live.Listen), EnvHint: envHint(live.Mode, live.Listen), PID: live.PID, State: p.State, Log: p.Log}
			if live.Listen != listen || live.Upstream != upstream {
				return Result{Data: data}, diag(DiagArgsDiffer, fmt.Sprintf("%s is already up as %s → %s", name, live.Listen, live.Upstream),
					"blip down "+name+" && blip up "+name+" --listen "+listen+" --to "+to)
			}
			return Result{Data: data, Lines: []string{fmt.Sprintf("%s already up: %s %s → %s", name, mode, data.URL, upstream)},
				Diagnostics: []runx.Diagnostic{info(DiagAlreadyUp, "same listen and upstream; nothing to do", "")}, Next: next}, nil
		}
		return Result{}, diag(DiagStale, fmt.Sprintf("%s has a state file (pid %d) but no daemon answers", name, st.PID), "blip down "+name)
	}
	if err := os.MkdirAll(t.Dir, 0o700); err != nil {
		return Result{}, err
	}
	exe, err := os.Executable()
	if err != nil {
		return Result{}, err
	}
	logF, err := os.OpenFile(p.Log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return Result{}, err
	}
	defer logF.Close()
	args := append(append([]string{}, t.Self...), "serve", "--name", name, "--listen", listen, "--to", to)
	cmd := exec.Command(exe, args...)
	cmd.Stdout, cmd.Stderr, cmd.Stdin = logF, logF, nil
	cmd.Env = append(os.Environ(), "BLIP_STATE_DIR="+t.Dir)
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return Result{}, err
	}
	_ = cmd.Process.Release()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if live, err := dial(p.Sock).status(); err == nil {
			data := upData{Config: cfg, URL: listenURL(mode, listen), EnvHint: envHint(mode, listen), PID: live.PID, State: p.State, Log: p.Log}
			return Result{Data: data, Lines: []string{
				fmt.Sprintf("%s up: %s %s → %s (pid %d)", name, mode, data.URL, upstream, live.PID),
				"point the app at " + data.URL + "  (" + data.EnvHint + ")",
			}, Next: next}, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = os.Remove(p.State)
	return Result{}, diag(DiagStartFailed, "daemon did not answer within 3s; last log lines: "+lastLines(p.Log, 3), "blip up "+name+" --listen "+listen+" --to "+to)
}

func lastLines(path string, n int) string {
	lines, err := tailLines(path, n)
	if err != nil || len(lines) == 0 {
		return "(no log)"
	}
	return strings.Join(lines, " | ")
}

// tailLines returns the last n lines of path, reading backwards from the
// end in fixed chunks so a long-running daemon's log never has to fit in
// memory. n <= 0 returns nothing; a missing file is an empty tail.
func tailLines(path string, n int) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	size, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, err
	}
	const chunk = 8 * 1024
	var tail []byte
	for off := size; off > 0 && bytes.Count(tail, []byte{'\n'}) <= n; {
		read := int64(chunk)
		if off < read {
			read = off
		}
		off -= read
		buf := make([]byte, read)
		if _, err := f.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		tail = append(buf, tail...)
	}
	text := strings.TrimRight(string(tail), "\n")
	if text == "" {
		return nil, nil
	}
	lines := strings.Split(text, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines, nil
}

// Down stops a proxy and removes its files; a dead daemon is cleaned too.
func (t *Tool) Down(name string) (Result, error) {
	if err := validName(name); err != nil {
		return Result{}, err
	}
	p := t.paths(name)
	st, err := readState(p.State)
	if errors.Is(err, os.ErrNotExist) {
		return Result{Data: map[string]any{"name": name, "was_running": false}, Lines: []string{name + ": nothing to take down"}, Next: []string{"blip ls"}}, nil
	}
	stopped := false
	if err == nil {
		if _, _, cerr := dial(p.Sock).call("POST", "/down", nil); cerr == nil {
			stopped = true
			for i := 0; i < 40 && fileExists(p.State); i++ {
				time.Sleep(50 * time.Millisecond)
			}
		} else if st.PID > 0 && ownsPID(st.PID, name) {
			_ = terminate(st.PID)
		}
	}
	for _, f := range []string{p.State, p.Sock, p.Log, p.Rec} {
		_ = os.Remove(f)
	}
	line := name + ": down"
	if !stopped {
		line = name + ": stale state cleaned"
	}
	return Result{Data: map[string]any{"name": name, "was_running": stopped}, Lines: []string{line}, Next: []string{"blip ls"}}, nil
}

// DownAll takes every known proxy down.
func (t *Tool) DownAll() (Result, error) {
	names, err := t.names()
	if err != nil {
		return Result{}, err
	}
	var lines []string
	var downed []string
	for _, n := range names {
		r, err := t.Down(n)
		if err != nil {
			return Result{}, err
		}
		lines = append(lines, r.Lines...)
		downed = append(downed, n)
	}
	if len(downed) == 0 {
		lines = []string{"no proxies up"}
	}
	return Result{Data: map[string]any{"down": downed}, Lines: lines, Next: []string{"blip up <name> --listen <addr> --to <upstream>"}}, nil
}

func (t *Tool) names() ([]string, error) {
	matches, err := filepath.Glob(filepath.Join(t.Dir, "*.json"))
	if err != nil {
		return nil, err
	}
	var names []string
	for _, m := range matches {
		names = append(names, strings.TrimSuffix(filepath.Base(m), ".json"))
	}
	sort.Strings(names)
	return names, nil
}

type lsRow struct {
	Name     string `json:"name"`
	Mode     string `json:"mode,omitempty"`
	Listen   string `json:"listen,omitempty"`
	Upstream string `json:"upstream,omitempty"`
	Fault    string `json:"fault,omitempty"`
	PID      int    `json:"pid,omitempty"`
	Alive    bool   `json:"alive"`
}

// Ls lists proxies, marking dead daemons.
func (t *Tool) Ls() (Result, error) {
	names, err := t.names()
	if err != nil {
		return Result{}, err
	}
	rows := []lsRow{}
	var lines []string
	var diags []runx.Diagnostic
	for _, n := range names {
		p := t.paths(n)
		st, err := readState(p.State)
		if err != nil {
			continue
		}
		row := lsRow{Name: n, Mode: st.Mode, Listen: st.Listen, Upstream: st.Upstream, PID: st.PID}
		if live, err := dial(p.Sock).status(); err == nil {
			row.Alive = true
			row.Fault = live.Fault.String()
			lines = append(lines, fmt.Sprintf("%s  %s %s → %s  fault=%s", n, st.Mode, listenURL(st.Mode, st.Listen), st.Upstream, row.Fault))
		} else {
			lines = append(lines, fmt.Sprintf("%s  STALE (pid %d dead)", n, st.PID))
			diags = append(diags, runx.Diagnostic{Code: DiagStale, Severity: "warning", Detail: n + " has a state file but no daemon", Fix: "blip down " + n})
		}
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		lines = []string{"no proxies up"}
	}
	next := []string{"blip up <name> --listen <addr> --to <upstream>"}
	if len(rows) > 0 {
		next = []string{"blip " + rows[0].Name + " status", "blip down --all"}
	}
	return Result{Data: map[string]any{"proxies": rows}, Lines: lines, Diagnostics: diags, Next: next}, nil
}

// Set replaces the active fault.
func (t *Tool) Set(name string, args []string, flags ScopeFlags) (Result, error) {
	_, st, c, err := t.connect(name)
	if err != nil {
		return Result{}, err
	}
	f, err := ParseFault(name, st.Mode, args, flags)
	if err != nil {
		return Result{}, err
	}
	live, _, err := c.call("POST", "/set", f)
	if err != nil {
		return Result{}, err
	}
	return Result{Data: live, Lines: []string{name + " fault: " + live.Fault.String()},
		Next: []string{"blip " + name + " ok", "blip " + name + " log --tail"}}, nil
}

// Ok clears the fault.
func (t *Tool) Ok(name string) (Result, error) {
	_, _, c, err := t.connect(name)
	if err != nil {
		return Result{}, err
	}
	live, _, err := c.call("POST", "/ok", nil)
	if err != nil {
		return Result{}, err
	}
	return Result{Data: live, Lines: []string{name + " healed: no fault"}, Next: []string{"blip " + name + " log --tail", "blip " + name + " set delay 800ms"}}, nil
}

// Cut closes every established connection once.
func (t *Tool) Cut(name string) (Result, error) {
	_, _, c, err := t.connect(name)
	if err != nil {
		return Result{}, err
	}
	live, hdr, err := c.call("POST", "/cut", nil)
	if err != nil {
		return Result{}, err
	}
	n := hdr.Get("X-Blip-Cut")
	return Result{Data: map[string]any{"cut": n, "status": live}, Lines: []string{name + ": cut " + n + " connection(s); fault unchanged (" + live.Fault.String() + ")"},
		Next: []string{"blip " + name + " log --tail", "blip " + name + " status"}}, nil
}

// Status reports the proxy.
func (t *Tool) Status(name string) (Result, error) {
	_, _, c, err := t.connect(name)
	if err != nil {
		return Result{}, err
	}
	live, err := c.status()
	if err != nil {
		return Result{}, err
	}
	k := live.Counters
	lines := []string{
		fmt.Sprintf("%s  %s %s → %s  (pid %d, up %s)", name, live.Mode, listenURL(live.Mode, live.Listen), live.Upstream, live.PID, time.Since(live.StartedAt).Round(time.Second)),
		"fault: " + live.Fault.String(),
		fmt.Sprintf("requests=%d faulted=%d in_flight=%d connections=%d recorded=%d recording=%t", k.Requests, k.Faulted, k.InFlight, k.Connections, k.Recorded, live.Recording),
	}
	if len(live.CredentialHeaders) > 0 {
		lines = append(lines, "recorded flow used credential headers: "+strings.Join(live.CredentialHeaders, ", ")+" (values not stored)")
	}
	var diags []runx.Diagnostic
	if live.Fault == nil {
		diags = append(diags, info("NO_FAULT", "passing through", ""))
	}
	next := []string{"blip " + name + " set delay 800ms", "blip " + name + " log"}
	if live.Fault != nil {
		next = []string{"blip " + name + " ok", "blip " + name + " log --tail"}
	}
	return Result{Data: live, Lines: lines, Diagnostics: diags, Next: next}, nil
}

// Log returns the last `last` lines, or — when tail is set — hands the
// existing tail and then every new line to emit until stop closes (the CLI
// decides whether a line is plain text or an NDJSON event).
func (t *Tool) Log(name string, last int, tail bool, emit func(line string), stop <-chan struct{}) (Result, error) {
	p, _, _, err := t.connect(name)
	if err != nil {
		return Result{}, err
	}
	lines, err := tailLines(p.Log, last)
	if err != nil {
		return Result{}, err
	}
	if lines == nil {
		lines = []string{}
	}
	if !tail {
		return Result{Data: map[string]any{"file": p.Log, "lines": lines}, Lines: lines, Next: []string{"blip " + name + " log --tail", "blip " + name + " status"}}, nil
	}
	for _, l := range lines {
		emit(l)
	}
	f, err := os.Open(p.Log)
	if err != nil {
		return Result{}, err
	}
	defer f.Close()
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return Result{}, err
	}
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadString('\n')
		if line != "" {
			emit(strings.TrimRight(line, "\n"))
		}
		if err != nil {
			select {
			case <-stop:
				return Result{Data: map[string]any{"file": p.Log}, Next: []string{"blip " + name + " status"}}, nil
			case <-time.After(200 * time.Millisecond):
			}
			if !fileExists(p.Log) {
				return Result{Data: map[string]any{"file": p.Log}, Diagnostics: []runx.Diagnostic{info(DiagNotRunning, name+" went down", "")}, Next: []string{"blip ls"}}, nil
			}
		}
	}
}

// Record toggles or clears the request recording (HTTP mode).
func (t *Tool) Record(name, action string) (Result, error) {
	_, st, c, err := t.connect(name)
	if err != nil {
		return Result{}, err
	}
	if st.Mode != "http" {
		return Result{}, diag(DiagHTTPOnly, "record stores HTTP requests; "+name+" is a tcp proxy", "blip "+name+" status")
	}
	var body map[string]any
	var line string
	var next []string
	switch action {
	case "on":
		body, line, next = map[string]any{"on": true}, name+": recording on", []string{"blip " + name + " record off"}
	case "off":
		body, line, next = map[string]any{"on": false}, name+": recording off", []string{"blip " + name + " authz --as none"}
	case "clear":
		body, line, next = map[string]any{"clear": true}, name+": recording cleared", []string{"blip " + name + " record on"}
	default:
		return Result{}, diag(DiagFaultInvalid, "record takes on, off or clear", "blip "+name+" record on")
	}
	live, _, err := c.call("POST", "/record", body)
	if err != nil {
		return Result{}, err
	}
	return Result{Data: live, Lines: []string{line, fmt.Sprintf("recorded=%d", live.Counters.Recorded)}, Next: next}, nil
}

// Authz replays the recording against the upstream with a substituted
// identity and reports what was not refused.
func (t *Tool) Authz(name string, opts AuthzOptions) (Result, error) {
	p, st, _, err := t.connect(name)
	if err != nil {
		return Result{}, err
	}
	if st.Mode != "http" {
		return Result{}, diag(DiagHTTPOnly, "authz replays HTTP requests; "+name+" is a tcp proxy", "blip "+name+" status")
	}
	recs, err := ReadRecords(p.Rec)
	if err != nil {
		return Result{}, err
	}
	if len(recs) == 0 {
		return Result{}, diag(DiagNothingRecorded, "nothing recorded for "+name+" yet", "blip "+name+" record on")
	}
	rep, err := Replay(st.Upstream, recs, opts)
	if err != nil {
		return Result{Data: rep}, err
	}
	return AuthzResult(name, rep)
}

// AuthzResult renders a report as a Result (exit 2 when candidates exist).
func AuthzResult(name string, rep AuthzReport) (Result, error) {
	var lines []string
	for _, c := range rep.Candidates {
		lines = append(lines, fmt.Sprintf("LEAK? %s %s  original=%d replayed=%d  %s (%d vs %d bytes)", c.Method, c.Path, c.Original, c.Replayed, c.Verdict, c.OriginalBytes, c.ReplayedBytes))
	}
	for _, s := range rep.Skipped {
		lines = append(lines, fmt.Sprintf("skipped %s %s: %s", s.Method, s.Path, s.Reason))
	}
	lines = append(lines, fmt.Sprintf("checked=%d candidates=%d skipped=%d as %s", rep.Checked, len(rep.Candidates), len(rep.Skipped), rep.Identity))
	next := []string{"blip " + name + " record clear"}
	res := Result{Data: rep, Lines: lines, Next: next}
	if len(rep.Candidates) > 0 {
		res.Diagnostics = []runx.Diagnostic{{Code: DiagAuthzLeakCandidates, Severity: "error",
			Detail: fmt.Sprintf("%d request(s) answered outside --expect with the substituted identity", len(rep.Candidates)), Fix: "blip " + name + " authz --as none --json"}}
		return res, runx.ExitError{Code: 2}
	}
	return res, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

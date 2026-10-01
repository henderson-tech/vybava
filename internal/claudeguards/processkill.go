package claudeguards

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/henderson-tech/vybava/internal/shellseg"
)

// ---------------------------------------------------------------------------
// process:pattern-kill / process:session-kill - a signal sent to processes
// picked by their command-line text, or to an agent session. On this Mac every
// Claude Code session runs as `bun … switcheroo start …` and every Codex
// sidekick as `switcheroo codex run …` over `codex …`. On 2026-10-01 a FixIt
// session stopping its own Codex sidekick and Appium run sent
//
//	pids=$(ps -axo pid,command | grep -E 'switcheroo|codex exec|…' | grep -v grep | awk '{print $1}'); kill -INT ${=pids}
//
// and SIGINT reached 57 processes: the user's other Claude Code sessions,
// their Codex runs and the calling session itself.
//
// pattern-kill reads the command alone: pkill/killall with any pattern,
// fuser -k, and a kill (or xargs kill) in a command that also lists processes
// (ps, pgrep, pidof, lsof, fuser) when its PIDs arrive through a substitution,
// a variable, xargs or a loop. session-kill resolves a kill's literal PIDs and
// -PGID groups against one `ps` read: a Claude Code or Codex session, the
// switcheroo or cmux launcher holding one, or any ancestor of one is refused
// unless it descends from the session the hook runs under, so a session may
// still stop its own sidekick by PID. Signal 0 (a probe), kill -l, job specs
// and $! pass, as does every PID the table cannot place. No escape: listing,
// reading the list and killing literal PIDs costs one more call, and a
// session that is not this one's is the user's to end.
// ---------------------------------------------------------------------------

var (
	// killListers print PIDs chosen by name, pattern, port or file.
	killListers = map[string]bool{"ps": true, "pgrep": true, "pidof": true, "lsof": true, "fuser": true}
	// killSenders choose their own targets (pkill, killall, fuser -k) or take
	// PIDs (kill).
	killSenders = map[string]bool{"kill": true, "pkill": true, "killall": true}
	// killLaunchers run the next command word on this machine: `sudo kill`,
	// `xargs -r kill -9`, `timeout 5 pkill`. Container and remote runners are
	// not here; their quoted payloads are segments of their own.
	killLaunchers = map[string]bool{
		"sudo": true, "doas": true, "xargs": true, "timeout": true, "gtimeout": true,
		"nice": true, "env": true, "command": true, "builtin": true, "exec": true,
		"nohup": true, "time": true, "stdbuf": true, "arch": true,
	}
	// shellKeywords open a compound command: `do kill $p` runs kill.
	shellKeywords = map[string]bool{"do": true, "then": true, "else": true, "elif": true, "if": true, "while": true, "until": true, "!": true, "{": true}
	// shellseg does not split a process substitution, so the lister inside
	// `done < <(pgrep -f x)` is found here.
	reProcSubLister = regexp.MustCompile(`<\([[:space:]]*(?:sudo[[:space:]]+)?(?:[^[:space:]()]*/)?(?:ps|pgrep|pidof|lsof|fuser)([[:space:]]|\)|$)`)
	// reRedirect is a redirection word (`2>/dev/null`, `>&2`, `&>log`), never
	// an operand.
	reRedirect  = regexp.MustCompile(`^(&>>?|[0-9]*(>>?|<<?|>&|<&|>\|))`)
	reLiteralID = regexp.MustCompile(`^-?[0-9]+$`)
)

// killInvocation is the command a segment runs once shell keywords and local
// launchers are peeled off, with its arguments, and whether xargs feeds it.
func killInvocation(seg string) (word string, args []string, viaXargs bool) {
	toks := shellseg.Fields(seg)
	i := 0
	for i < len(toks) && shellKeywords[toks[i]] {
		i++
	}
	if i >= len(toks) {
		return "", nil, false
	}
	w := path.Base(toks[i])
	if !killLaunchers[w] {
		return w, toks[i+1:], false
	}
	if w == "command" && i+1 < len(toks) && (toks[i+1] == "-v" || toks[i+1] == "-V") {
		return "", nil, false // a lookup, not a run
	}
	viaXargs = w == "xargs"
	for j := i + 1; j < len(toks); j++ {
		t := path.Base(toks[j])
		if killSenders[t] || killListers[t] {
			return t, toks[j+1:], viaXargs
		}
		if t == "xargs" {
			viaXargs = true
		}
		if shellseg.Runners[t] && !killLaunchers[t] {
			return "", nil, false // `xargs docker kill`: the runner's business, not a process signal
		}
	}
	return w, toks[i+1:], false
}

// killArgs reads kill's argv: the signal named (or ""), whether it only lists
// signals, and the operands. Once a signal is named a `-N` is an operand, a
// process group: `kill -9 -1` signals every process the user owns.
func killArgs(args []string) (sig string, list bool, ops []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return sig, false, args[i+1:]
		case a == "-l" || a == "-L" || a == "--list" || strings.HasPrefix(a, "--list="):
			return "", true, nil
		case a == "-s" || a == "-n" || a == "--signal":
			if i+1 < len(args) {
				sig = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--signal="):
			sig = strings.TrimPrefix(a, "--signal=")
		case sig == "" && len(a) > 1 && a[0] == '-':
			sig = a[1:]
		default:
			return sig, false, args[i:]
		}
	}
	return sig, false, nil
}

// killOperands drops redirections (`2>/dev/null`, `> log`) from kill's
// operands.
func killOperands(ops []string) []string {
	var out []string
	for i := 0; i < len(ops); i++ {
		if m := reRedirect.FindString(ops[i]); m != "" {
			if m == ops[i] {
				i++ // `2> /dev/null`: the target is the next word
			}
			continue
		}
		out = append(out, ops[i])
	}
	return out
}

// ownOperand is a target the calling shell started: a job spec or $!.
func ownOperand(op string) bool {
	return strings.HasPrefix(op, "%") || op == "$!" || op == "${!}"
}

// patternSignalsNothing reports a pkill/killall that sends no signal: signal
// 0, help or version, killall's signal list.
func patternSignalsNothing(word string, args []string) bool {
	for i, a := range args {
		switch a {
		case "-0", "--signal=0", "--help", "-V", "--version":
			return true
		case "-l", "--list":
			if word == "killall" {
				return true
			}
		case "-s", "--signal":
			if i+1 < len(args) && args[i+1] == "0" {
				return true
			}
		}
	}
	return false
}

// fuserKills reports fuser's -k (kill every process using the file or port).
func fuserKills(args []string) bool {
	for _, a := range args {
		if a == "--kill" || (len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsRune(a, 'k')) {
			return true
		}
	}
	return false
}

// substitutionFollows reports whether the shell text right after seg opens a
// command substitution: `kill 123 $(pgrep x)` reaches the rules as the
// segments `kill 123` and `pgrep x`.
func substitutionFollows(cmd, seg string) bool {
	i := strings.Index(cmd, seg)
	if i < 0 {
		return false
	}
	rest := strings.TrimLeft(cmd[i+len(seg):], " \t\"")
	return strings.HasPrefix(rest, "$(") || strings.HasPrefix(rest, "`")
}

// killPlan is what one command would signal, read without running anything:
// pattern is the segment that picks its targets by text ("" when none does),
// pids the literal operands of every other kill (negative: a process group).
type killPlan struct {
	pattern string
	pids    []int
}

func planKill(cmd string) killPlan {
	type sender struct {
		seg      string
		ops      []string
		viaXargs bool
	}
	var plan killPlan
	var senders []sender
	listed := false
	for _, seg := range shellseg.LocalSegments(cmd) {
		if textOnly(seg) {
			continue
		}
		if reProcSubLister.MatchString(seg) {
			listed = true
		}
		word, args, viaXargs := killInvocation(seg)
		switch word {
		case "pkill", "killall":
			if plan.pattern == "" && !patternSignalsNothing(word, args) {
				plan.pattern = seg
			}
		case "fuser":
			if fuserKills(args) {
				if plan.pattern == "" {
					plan.pattern = seg
				}
			} else {
				listed = true
			}
		case "ps", "pgrep", "pidof", "lsof":
			listed = true
		case "kill":
			sig, list, ops := killArgs(args)
			if list || sig == "0" {
				continue // a probe or the signal list sends nothing
			}
			senders = append(senders, sender{seg, killOperands(ops), viaXargs})
		}
	}
	for _, s := range senders {
		selected := s.viaXargs || len(s.ops) == 0 || substitutionFollows(cmd, s.seg)
		for _, op := range s.ops {
			if ownOperand(op) {
				continue
			}
			if !reLiteralID.MatchString(op) {
				selected = true
				continue
			}
			if n, err := strconv.Atoi(op); err == nil {
				plan.pids = append(plan.pids, n)
			}
		}
		if listed && selected && plan.pattern == "" {
			plan.pattern = s.seg
		}
	}
	return plan
}

// procRow is one row of `ps -A -o pid=,ppid=,pgid=,args=`.
type procRow struct {
	machineProc
	pgid int
}

// killProcTable reads the live table the session check resolves against;
// tests inject their own.
var killProcTable = func() []procRow {
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,pgid=,args=").Output()
	if err != nil {
		return nil
	}
	return parseKillTable(string(out))
}

// hookPID is where the walk to the calling session starts; tests set it.
var hookPID = os.Getpid

func parseKillTable(out string) []procRow {
	var rows []procRow
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		pgid, err3 := strconv.Atoi(f[2])
		if err1 != nil || err2 != nil || err3 != nil {
			continue
		}
		rows = append(rows, procRow{machineProc: machineProc{pid: pid, ppid: ppid, args: strings.Join(f[3:], " ")}, pgid: pgid})
	}
	return rows
}

// agentName names an agent session or launcher by its executable or script.
func agentName(base, full string) string {
	switch {
	case base == "claude" || strings.Contains(full, "@anthropic-ai/claude-code/"):
		return "a Claude Code session"
	case base == "codex" || strings.Contains(full, "@openai/codex/"):
		return "a Codex session"
	case base == "switcheroo":
		return "the switcheroo launcher of a session"
	case strings.HasPrefix(base, "cmux-") && strings.HasSuffix(base, "-wrapper"):
		return "a cmux agent wrapper"
	}
	return ""
}

// sessionKind names a process that is an agent session or the launcher
// holding one, "" for anything else. The executable, or the script a
// node/bun/shell runs, decides; never a substring of the whole argv, so
// `tail -f claude.log` is no session.
func sessionKind(p machineProc) string {
	if k := agentName(p.base(), p.exe()); k != "" {
		return k
	}
	switch p.base() {
	case "node", "bun", "sh", "bash", "zsh":
		s := nodeScript(p.args)
		return agentName(path.Base(s), s)
	}
	return ""
}

// sessionTarget is the first PID or group in pids whose signal would reach
// an agent session outside the calling one, with the process it would hit
// and why. ok is false when every target is safe or unknown.
func sessionTarget(pids []int, table []procRow, self int) (target int, hit procRow, why string, ok bool) {
	byPID := make(map[int]procRow, len(table))
	for _, r := range table {
		byPID[r.pid] = r
	}
	// root is the nearest session above the hook: its own tree is its own.
	root := 0
	for cur, hops := self, 0; cur > 1 && hops < 64; hops++ {
		r, found := byPID[cur]
		if !found {
			break
		}
		if sessionKind(r.machineProc) != "" {
			root = cur
			break
		}
		cur = r.ppid
	}
	underRoot := func(pid int) bool {
		if root == 0 {
			return false
		}
		for cur, hops := byPID[pid].ppid, 0; cur > 1 && hops < 64; hops++ {
			if cur == root {
				return true
			}
			r, found := byPID[cur]
			if !found {
				return false
			}
			cur = r.ppid
		}
		return false
	}
	// holds maps every ancestor of a session to one session below it.
	holds := map[int]int{}
	for _, r := range table {
		if sessionKind(r.machineProc) == "" {
			continue
		}
		for cur, hops := r.ppid, 0; cur > 0 && hops < 64; hops++ {
			if _, seen := holds[cur]; seen {
				break
			}
			holds[cur] = r.pid
			a, found := byPID[cur]
			if !found {
				break
			}
			cur = a.ppid
		}
	}
	for _, t := range pids {
		var members []procRow
		switch {
		case t > 0:
			if r, found := byPID[t]; found {
				members = append(members, r)
			}
		case t < -1:
			for _, r := range table {
				if r.pgid == -t || r.pid == -t {
					members = append(members, r)
				}
			}
		}
		for _, m := range members {
			if underRoot(m.pid) {
				continue
			}
			if k := sessionKind(m.machineProc); k != "" {
				return t, m, k, true
			}
			if s, held := holds[m.pid]; held {
				return t, m, fmt.Sprintf("an ancestor of the agent session %d (%s)", s, shortArgs(byPID[s].args, 60)), true
			}
		}
	}
	return 0, procRow{}, "", false
}

const killAlternative = `Stop only what this session started:
  • a background task: TaskStop with its task id
  • a process your own command started: kill $! or kill %1
  • anything else: list it, read the list, then kill the literal PIDs you checked
      pgrep -fl '<pattern>'      (or lsof -ti:<port>, ps -o pid,ppid,command -p <pid>)
      kill <pid> <pid>
    PIDs under this session pass; an agent session or its launcher is refused.
If the process belongs to another session or to the user, ask the user to stop it.`

func guardProcessKill(in *HookInput) *Denial {
	cmd := in.ToolInput.Command
	if !strings.Contains(cmd, "kill") && !strings.Contains(cmd, "fuser") {
		return nil
	}
	plan := planKill(cmd)
	if plan.pattern != "" {
		return deny("process:pattern-kill", fmt.Sprintf(`'%s' picks its targets by matching command-line text (pkill, killall, fuser -k,
or a ps/pgrep/pidof/lsof listing feeding kill). Every Claude Code session on this
Mac runs as 'bun … switcheroo start …' and every Codex sidekick as
'switcheroo codex run …' over 'codex …', so a pattern also matches the user's other
sessions, their sidekicks and this session itself: on 2026-10-01 one
'ps -axo pid,command | grep -E … | awk' feeding 'kill -INT' sent SIGINT to 57
processes.

%s`, shortArgs(plan.pattern, 160), killAlternative), "")
	}
	for _, p := range plan.pids {
		if p == -1 {
			return deny("process:session-kill", "kill -1 signals every process you own, every Claude Code and Codex session on this machine included.\n\n"+killAlternative, "")
		}
	}
	if len(plan.pids) == 0 {
		return nil
	}
	table := killProcTable()
	if table == nil {
		return nil // fail open: no table, no verdict
	}
	if t, hit, why, ok := sessionTarget(plan.pids, table, hookPID()); ok {
		target := fmt.Sprintf("PID %d", t)
		if t < 0 {
			target = fmt.Sprintf("process group %d, which holds PID %d", -t, hit.pid)
		}
		return deny("process:session-kill", fmt.Sprintf(`kill would signal %s (%s): %s.
Signalling it ends an agent session, this one or the user's, with every task,
subagent and sidekick under it.

%s`, target, shortArgs(hit.args, 100), why, killAlternative), "")
	}
	return nil
}

package claudeguards

import (
	"encoding/json"
	"io"
	"regexp"
	"strings"
)

// HookInput is the payload Claude Code pipes to hook commands — PreToolUse
// carries the tool fields, the session-lifecycle hooks carry session_id.
// Unknown fields are ignored by encoding/json, so schema growth is safe.
type HookInput struct {
	CWD            string `json:"cwd"`
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	ToolName       string `json:"tool_name"`
	ToolInput      struct {
		Command  string `json:"command"`
		FilePath string `json:"file_path"`
		Offset   int    `json:"offset"`
		Limit    int    `json:"limit"`
	} `json:"tool_input"`

	guardsCfg  *Config // memoized by guards(); never set from JSON
	budgetVal  Budget  // memoized by budget()
	budgetErr  error
	budgetRead bool
}

// budget returns this payload's context budget, reading the transcript at most
// once. guardBudget consults it, and on every allowed call BudgetContext then
// consults it again — at up to 4 MiB of JSON per read, that doubled the hook's
// I/O for an answer that cannot have changed in between.
func (in *HookInput) budget() (Budget, error) {
	if !in.budgetRead {
		in.budgetVal, in.budgetErr = ReadBudget(in.TranscriptPath)
		in.budgetRead = true
	}
	return in.budgetVal, in.budgetErr
}

// guards returns the repo's guards config, loading it at most once per hook
// payload. One Bash call runs several rules and many segments, and each used to
// re-run vconfig.Load with its own `git rev-parse`. Memoizing on the PAYLOAD
// rather than in a package variable keeps the rules free of process-global
// state, which was eve's standing ruling on #53.
func (in *HookInput) guards() Config {
	if in.guardsCfg == nil {
		cfg := guardConfig(in.CWD)
		in.guardsCfg = &cfg
	}
	return *in.guardsCfg
}

// ReadInput parses the hook payload; any error means fail-open.
func ReadInput(r io.Reader) (*HookInput, error) {
	raw, err := io.ReadAll(io.LimitReader(r, 4<<20))
	if err != nil {
		return nil, err
	}
	var in HookInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return nil, err
	}
	return &in, nil
}

// Shell separators after which a new command can start. Splitting on these
// means `make build && git stash` is inspected too. `$(` and backtick open
// command substitutions.
var segmentSplit = regexp.MustCompile("\\|\\||&&|[;&|\n]|\\$\\(|`")

// A heredoc intro with a QUOTED delimiter: `<< 'EOF'` / << "EOF" (optionally
// <<-). A quoted delimiter makes the body literal text — no expansion, no
// command substitution — so it cannot execute anything and is stripped before
// segmenting. Unquoted heredocs (<< EOF) DO expand `$(...)` in the body, so
// their bodies stay in and are scanned like any other text.
var heredocQuotedRE = regexp.MustCompile(`<<-?[ \t]*(?:'([A-Za-z_][A-Za-z0-9_]*)'|"([A-Za-z_][A-Za-z0-9_]*)")`)

// stripQuotedHeredocs removes the bodies (and closing delimiter lines) of
// quoted-delimiter heredocs so prose/scripts fed via stdin don't false-trip
// command rules. Content-preserving for everything else.
func stripQuotedHeredocs(cmd string) string {
	var b strings.Builder
	pos := 0
	for pos < len(cmd) {
		m := heredocQuotedRE.FindStringSubmatchIndex(cmd[pos:])
		if m == nil {
			b.WriteString(cmd[pos:])
			break
		}
		delim := ""
		if m[2] >= 0 {
			delim = cmd[pos+m[2] : pos+m[3]]
		} else {
			delim = cmd[pos+m[4] : pos+m[5]]
		}
		introEnd := pos + m[1]
		nl := strings.IndexByte(cmd[introEnd:], '\n')
		if nl < 0 {
			// Intro with no body in this string; keep everything.
			b.WriteString(cmd[pos:])
			break
		}
		bodyStart := introEnd + nl + 1
		// Keep everything through the intro line's newline.
		b.WriteString(cmd[pos:bodyStart])
		// Skip the body up to and including the closing delimiter line. If the
		// delimiter never appears, shell semantics make the whole rest the
		// body — skip it all.
		rest := cmd[bodyStart:]
		end := len(cmd)
		switch {
		case strings.HasPrefix(rest, delim+"\n"):
			end = bodyStart + len(delim) + 1
		case rest == delim:
			end = len(cmd)
		default:
			if i := strings.Index(rest, "\n"+delim+"\n"); i >= 0 {
				end = bodyStart + i + 1 + len(delim) + 1
			} else if strings.HasSuffix(rest, "\n"+delim) {
				end = len(cmd)
			}
		}
		pos = end
	}
	return b.String()
}

// segments splits a command string into independently inspectable pieces,
// left-trimmed, empties dropped. Quoted-heredoc bodies are excluded first.
// trimSubshell strips the wrapper a subshell leaves on a segment. segmentSplit
// breaks `(cd /tmp && git log -n 20)` into `(cd /tmp` and `git log -n 20)`, and
// the stray `)` made the last field "20)" — not a number, so the cap went
// unseen and context:unbounded-output fired on a correctly capped command.
// ~/.claude/CLAUDE.md REQUIRES `(cd <abs> && cmd)` for directory changes, so
// this is the common shape, not an edge case. Only unbalanced trailing parens
// are removed, leaving a legitimate `)` inside an argument alone.
func trimSubshell(s string) string {
	s = strings.TrimLeft(s, "( \t")
	depth := 0
	for _, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		}
	}
	for depth < 0 {
		trimmed := strings.TrimRight(s, " \t")
		if !strings.HasSuffix(trimmed, ")") {
			break
		}
		s = trimmed[:len(trimmed)-1]
		depth++
	}
	return strings.TrimRight(s, " \t")
}

func segments(cmd string) []string {
	parts := segmentSplit.Split(stripQuotedHeredocs(cmd), -1)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		s := trimSubshell(strings.Trim(p, " \t\r"))
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// textOnly reports whether a segment merely *mentions* commands without being
// able to execute them: comments, echoed/printed text, commit messages. Chained
// real commands live in their own segment, so skipping these costs no coverage.
func textOnly(s string) bool {
	if strings.HasPrefix(s, "#") {
		return true
	}
	for _, p := range []string{"echo ", "printf ", "cat ", "git commit "} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return s == "git commit"
}

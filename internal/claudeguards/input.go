package claudeguards

import (
	"encoding/json"
	"io"
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
		// Browser-MCP screenshot targets (screenshotdir.go): playwright's
		// browser_take_screenshot `filename`, chrome-devtools' take_screenshot
		// `filePath`.
		Filename      string `json:"filename"`
		FilePathCamel string `json:"filePath"`
	} `json:"tool_input"`

	guardsCfg  *Config // memoized by guards(); never set from JSON
	budgetVal  Budget  // memoized by budget()
	budgetErr  error
	budgetRead bool

	// Notes are one-line, model-visible remarks a rule attaches to a call it
	// LETS THROUGH (the devbox rule running a routed command here because the
	// box is saturated). The CLI delivers them as PreToolUse additionalContext
	// next to the budget reminder. A rule never notes a passing call it did
	// not otherwise judge: a remark on every call is the context cost the
	// budget rules exist to prevent.
	Notes []string
}

// note attaches one model-visible remark to this call.
func (in *HookInput) note(msg string) { in.Notes = append(in.Notes, msg) }

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

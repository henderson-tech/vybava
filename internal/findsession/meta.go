package findsession

import (
	"bufio"
	"bytes"
	"os"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/secretscan"
	"github.com/henderson-tech/vybava/internal/transcripts"
)

// Session is what a reader needs to recognise a session and reopen it.
type Session struct {
	ID        string    `json:"id"`
	Path      string    `json:"path"`
	Title     string    `json:"title,omitempty"`
	Prompt    string    `json:"prompt,omitempty"`
	Cwd       string    `json:"cwd,omitempty"`
	Branch    string    `json:"branch,omitempty"`
	Started   time.Time `json:"started"`
	Ended     time.Time `json:"ended"`
	Model     string    `json:"model,omitempty"`
	Effort    string    `json:"effort,omitempty"`
	Ultracode bool      `json:"ultracode"`
	// Found and Fragments say how many of the query's needles it holds;
	// Authored counts the hits in its own assistant turns (it wrote the
	// text), Titled is set when its title holds one, and Quoted counts the
	// hits anywhere else (a paste, a tool result).
	Found     int  `json:"found"`
	Fragments int  `json:"fragments"`
	Authored  int  `json:"authored"`
	Titled    bool `json:"titled,omitempty"`
	Quoted    int  `json:"quoted"`
	// LastHit is the line of the last authored hit; Lines the transcript's
	// length — equal-ish when the text is the session's ending.
	LastHit int    `json:"lastHit,omitempty"`
	Lines   int    `json:"lines"`
	Reopen  string `json:"reopen"`
	// CwdMissing is set when the launch directory is gone (a removed
	// worktree): resume looks the transcript up by that directory.
	CwdMissing bool `json:"cwdMissing,omitempty"`
}

// Byte prefilters pick the lines inspect decodes (through the transcripts
// package); everything else is only counted.
var (
	keyTimestamp = []byte(`"timestamp":"`)
	keyCwd       = []byte(`"cwd":"`)
	keyAssistant = []byte(`"type":"assistant"`)
	keyAITitle   = []byte(`"type":"ai-title"`)
	keyCustom    = []byte(`"type":"custom-title"`)
	keyUltra     = []byte(`"ultra_effort_enter"`)
)

// inspect reads one transcript line by line, so a large transcript costs
// one pass and a handful of decodes.
func inspect(file sessionFile, needles []needle) (Session, error) {
	s := Session{ID: file.ID, Path: file.Path, Fragments: len(needles)}
	f, err := os.Open(file.Path)
	if err != nil {
		return s, err
	}
	defer f.Close()
	reader := bufio.NewReaderSize(f, 1<<20)
	st := inspectState{}
	var lastStamped []byte
	for {
		raw, err := reader.ReadBytes('\n')
		if len(raw) > 0 {
			s.Lines++
			s.inspectLine(raw, needles, &st)
			if bytes.Contains(raw, keyTimestamp) {
				lastStamped = raw
			}
		}
		if err != nil {
			break
		}
	}
	if rec, err := transcripts.DecodeClaude(lastStamped); err == nil && !rec.Timestamp.IsZero() {
		s.Ended = rec.Timestamp
	}
	if st.customTitle != "" {
		s.Title = st.customTitle
	}
	// Title and prompt reach the output (an agent's context): never a credential.
	s.Title, s.Prompt = redactSecrets(s.Title), redactSecrets(s.Prompt)
	return s, nil
}

type inspectState struct {
	customTitle   string
	seenAssistant bool
}

func (s *Session) inspectLine(raw []byte, needles []needle, st *inspectState) {
	hit := false
	for _, needle := range needles {
		if needle.in(raw) {
			hit = true
			break
		}
	}
	want := hit ||
		s.Started.IsZero() && bytes.Contains(raw, keyTimestamp) ||
		s.Cwd == "" && bytes.Contains(raw, keyCwd) ||
		!st.seenAssistant && (bytes.Contains(raw, keyAssistant) || bytes.Contains(raw, keyUltra)) ||
		bytes.Contains(raw, keyAITitle) || bytes.Contains(raw, keyCustom) ||
		s.Prompt == "" && transcripts.ClaudeHumanLine(raw)
	if !want {
		return
	}
	rec, err := transcripts.DecodeClaude(raw)
	if err != nil {
		return
	}
	if s.Started.IsZero() && !rec.Timestamp.IsZero() {
		s.Started = rec.Timestamp
	}
	if s.Cwd == "" && rec.Cwd != "" {
		s.Cwd, s.Branch = rec.Cwd, rec.GitBranch
	}
	switch rec.Type {
	case "ai-title":
		s.Title = rec.AITitle
		s.Titled = s.Titled || hit && holdsAny(rec.AITitle, needles)
		return
	case "custom-title":
		st.customTitle = rec.CustomTitle
		s.Titled = s.Titled || hit && holdsAny(rec.CustomTitle, needles)
		return
	case "attachment":
		if !st.seenAssistant && rec.Attachment != nil && rec.Attachment.Type == "ultra_effort_enter" {
			s.Ultracode = true
		}
	case "assistant":
		if !st.seenAssistant && rec.Message.Model != transcripts.SyntheticModel {
			st.seenAssistant = true
			s.Model, s.Effort = rec.Message.Model, rec.Effort
		}
	}
	if s.Prompt == "" && rec.HumanPrompt() {
		if texts := rec.Message.Texts(); len(texts) > 0 {
			s.Prompt = clip(texts[0], 160)
		}
	}
	if !hit {
		return
	}
	// Authorship is the reply text alone: a tool call carrying the text (a
	// find-session query, a file write) is a quote.
	if rec.Type == "assistant" && !rec.IsSidechain && holdsAny(strings.Join(rec.Message.Texts(), "\n"), needles) {
		s.Authored++
		s.LastHit = s.Lines
	} else {
		s.Quoted++
	}
}

func holdsAny(text string, needles []needle) bool {
	for _, needle := range needles {
		if needle.in([]byte(text)) {
			return true
		}
	}
	return false
}

func clip(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit-1]) + "…"
}

// knownEfforts are the levels switcheroo --effort takes; a transcript value
// outside them never reaches the reopen line.
var knownEfforts = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true, "ultracode": true}

// Preset is the switcheroo launcher that starts a session the way this one
// started: the model and effort of its first reply, ultracode when it was on
// before that reply. Combinations without a preset spell the flags out.
func Preset(model, effort string, ultracode bool) string {
	family := ""
	switch {
	case strings.Contains(model, "opus"):
		family = "opus"
	case strings.Contains(model, "fable"):
		family = "fable"
	default:
		return "cc"
	}
	if ultracode {
		effort = "ultracode"
	}
	presets := map[string]string{
		"opus/ultracode": "ccoo",
		"opus/high":      "cco",
		"opus/low":       "ccol",
		"fable/high":     "cch",
		"fable/low":      "ccl",
	}
	if preset, ok := presets[family+"/"+effort]; ok {
		return preset
	}
	if !knownEfforts[effort] {
		return "cc --model " + family // none recorded, or one switcheroo would not take
	}
	return "cc --model " + family + " --effort " + effort
}

// reopenCommand is the one shell line that resumes the session from its
// launch directory under its preset; with no directory recorded it is the
// resume alone (CWD_UNKNOWN says where to run it).
func reopenCommand(s Session, home string) string {
	if s.Cwd == "" {
		return Preset(s.Model, s.Effort, s.Ultracode) + " -- --resume " + s.ID
	}
	return "cd " + shellPath(s.Cwd, home) + " && " + Preset(s.Model, s.Effort, s.Ultracode) + " -- --resume " + s.ID
}

// shellPath writes a path for a shell: ~-relative under home, single-quoted
// when it holds anything a shell would split or expand.
func shellPath(path, home string) string {
	if home != "" && path == home {
		return "~"
	}
	prefix, rest := "", path
	if home != "" && strings.HasPrefix(path, home+"/") {
		prefix, rest = "~/", strings.TrimPrefix(path, home+"/")
	}
	if strings.IndexFunc(rest, unsafeRune) >= 0 {
		rest = "'" + strings.ReplaceAll(rest, "'", `'\''`) + "'"
	}
	return prefix + rest
}

func unsafeRune(r rune) bool {
	return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+,", r))
}

// Own reports whether the session wrote the text — in a reply or its title —
// rather than only quoting it.
func (s Session) Own() bool { return s.Authored > 0 || s.Titled }

func redactSecrets(text string) string {
	return secretscan.Redact(text, secretscan.Find(text, secretscan.All, nil))
}

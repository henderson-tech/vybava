package fleet

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// DialogKind says what an open dialog asks.
type DialogKind string

const (
	// DialogPermission is Claude Code's tool-permission prompt ("Do you want to proceed?").
	DialogPermission DialogKind = "permission"
	// DialogQuestion is an AskUserQuestion prompt.
	DialogQuestion DialogKind = "question"
)

// OptionKind says what choosing an option does.
type OptionKind string

const (
	// OptionAnswer answers the dialog.
	OptionAnswer OptionKind = "answer"
	// OptionOther opens a free-text field inside the dialog ("Type something.");
	// fleet never offers it — typed text stays in the reply path's rules.
	OptionOther OptionKind = "other"
	// OptionChat leaves the dialog for a plain conversation ("Chat about this").
	OptionChat OptionKind = "chat"
)

// Dialog is an open permission or question prompt parsed from a screen.
type Dialog struct {
	Kind     DialogKind `json:"kind"`
	Title    string     `json:"title,omitempty"`
	Question string     `json:"question"`
	// Detail is what a permission prompt shows between its title and the
	// question: the command, the file, the tool's description.
	Detail  string         `json:"detail,omitempty"`
	Tabs    []DialogTab    `json:"tabs,omitempty"`
	Options []DialogOption `json:"options"`
	// Answerable is false when one key press cannot answer it: a
	// multi-select list, or an option numbered past 9. Fleet then offers
	// Focus only.
	Answerable bool `json:"answerable"`
	// Fingerprint names this exact dialog; `fleet reply --option` refuses
	// when the screen no longer shows the one the human chose from.
	Fingerprint string `json:"fingerprint"`
}

// DialogTab is one question of a multi-question AskUserQuestion.
type DialogTab struct {
	Label string `json:"label"`
	Done  bool   `json:"done"`
}

// DialogOption is one numbered choice; Key is the key that picks it.
type DialogOption struct {
	Key         string     `json:"key"`
	Label       string     `json:"label"`
	Description string     `json:"description,omitempty"`
	Kind        OptionKind `json:"kind"`
	Selected    bool       `json:"selected"`
}

var (
	optionLine = regexp.MustCompile(`^(\s*)(❯\s*)?(\d+)\.\s+(.+)$`)
	checkbox   = regexp.MustCompile(`^\[[ x✓✔]\]\s`)
)

// footerWindow is how many non-blank lines from the bottom the dialog's
// "Esc to cancel" footer may sit: an open dialog owns the bottom of the
// screen, so an old dialog in scrollback never matches.
const footerWindow = 2

// ParseDialog finds the dialog open at the bottom of a Claude Code screen,
// or nil when the bottom is anything else (the prompt, a running turn).
func ParseDialog(screen string) *Dialog {
	lines := strings.Split(strings.ReplaceAll(screen, "\r", ""), "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t ")
	}
	footer := -1
	seen := 0
	for i := len(lines) - 1; i >= 0 && seen < footerWindow; i-- {
		if lines[i] == "" {
			continue
		}
		seen++
		if strings.Contains(lines[i], "Esc to cancel") {
			footer = i
			break
		}
	}
	if footer < 0 {
		return nil
	}

	// Options, bottom-up: numbered lines, their indented descriptions, and
	// the rule Claude Code draws between "Type something." and the rest.
	var options []DialogOption
	var pending []string
	top := footer
	for i := footer - 1; i >= 0; i-- {
		line := lines[i]
		if m := optionLine.FindStringSubmatch(line); m != nil {
			option := DialogOption{Key: m[3], Label: strings.TrimSpace(m[4]), Selected: m[2] != ""}
			if len(pending) > 0 {
				option.Description = strings.Join(reverse(pending), " ")
				pending = nil
			}
			options = append(options, option)
			top = i
			continue
		}
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || isRule(trimmed) {
			continue
		}
		if indent(line) >= 4 && !strings.HasPrefix(trimmed, "│") {
			pending = append(pending, trimmed)
			continue
		}
		break
	}
	if len(options) == 0 {
		return nil
	}
	options = reverseOptions(options)

	dialog := &Dialog{Options: options, Answerable: true}
	for i := range dialog.Options {
		option := &dialog.Options[i]
		switch {
		case strings.HasPrefix(option.Label, "Type something"):
			option.Kind = OptionOther
		case option.Label == "Chat about this":
			option.Kind = OptionChat
		default:
			option.Kind = OptionAnswer
		}
		if checkbox.MatchString(option.Label) || len(option.Key) > 1 {
			dialog.Answerable = false
		}
	}

	// Above the options: the question and what frames it.
	above := nonBlankAbove(lines, top, frameLines)
	switch {
	case len(above) > 0 && strings.HasPrefix(above[0], "│"):
		dialog.Kind = DialogQuestion
		var question []string
		rest := above
		for len(rest) > 0 && strings.HasPrefix(rest[0], "│") {
			question = append(question, strings.TrimSpace(strings.TrimPrefix(rest[0], "│")))
			rest = rest[1:]
		}
		dialog.Question = strings.Join(reverse(question), " ")
		if len(rest) > 0 && strings.HasPrefix(rest[0], "←") {
			dialog.Tabs = parseTabs(rest[0])
		}
	case len(above) > 0:
		dialog.Kind = DialogPermission
		dialog.Question = above[0]
		var block []string
		for _, line := range above[1:] {
			if isRule(line) && !strings.HasPrefix(line, "╌") {
				break
			}
			block = append(block, line)
		}
		block = reverse(block)
		if len(block) > 0 {
			dialog.Title = block[0]
			var detail []string
			for _, line := range block[1:] {
				if isRule(line) || strings.HasPrefix(line, "Tip:") {
					continue
				}
				detail = append(detail, line)
			}
			dialog.Detail = strings.Join(detail, "\n")
		}
	default:
		return nil
	}
	dialog.Fingerprint = dialog.fingerprint()
	return dialog
}

// promptWindow is how many non-blank lines from the bottom the input box may
// sit: below it Claude Code draws its status and mode lines.
const promptWindow = 8

// AtPrompt reports whether the bottom of a Claude Code screen is the
// conversation's own input box — a `❯` line between two rules — and not the
// agents view, whose identical box starts a NEW background session (its
// hint line reads "enter to open · space to reply").
func AtPrompt(screen string) bool {
	lines := strings.Split(strings.ReplaceAll(screen, "\r", ""), "\n")
	bottom := nonBlankAbove(lines, len(lines), promptWindow)
	for i, line := range bottom {
		if !strings.HasPrefix(line, "❯") || i == 0 || i+1 >= len(bottom) || !isRule(bottom[i-1]) || !isRule(bottom[i+1]) {
			continue
		}
		for _, below := range bottom[:i] {
			if strings.Contains(below, "enter to open") || strings.Contains(below, "space to reply") {
				return false
			}
		}
		return true
	}
	return false
}

func (d *Dialog) fingerprint() string {
	h := sha256.New()
	for _, part := range []string{string(d.Kind), d.Title, d.Question, d.Detail} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	for _, o := range d.Options {
		h.Write([]byte(o.Key + "\x00" + o.Label + "\x00" + o.Description + "\x00"))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// Option returns the answerable option a key picks.
func (d *Dialog) Option(key string) (DialogOption, bool) {
	for _, o := range d.Options {
		if o.Key == key && o.Kind != OptionOther {
			return o, true
		}
	}
	return DialogOption{}, false
}

func parseTabs(line string) []DialogTab {
	var tabs []DialogTab
	for _, field := range strings.Split(strings.Trim(line, "←→ "), "  ") {
		field = strings.TrimSpace(field)
		mark, label, ok := strings.Cut(field, " ")
		if !ok {
			continue
		}
		switch mark {
		case "☐":
			tabs = append(tabs, DialogTab{Label: strings.TrimSpace(label)})
		case "☒", "✔", "✓":
			if strings.TrimSpace(label) != "Submit" {
				tabs = append(tabs, DialogTab{Label: strings.TrimSpace(label), Done: true})
			}
		}
	}
	return tabs
}

// frameLines bounds how far above its options a dialog's question, title and
// detail are looked for; past it is the transcript.
const frameLines = 16

// nonBlankAbove returns up to max trimmed non-blank lines above index i,
// nearest first.
func nonBlankAbove(lines []string, i, max int) []string {
	var out []string
	for j := i - 1; j >= 0 && len(out) < max; j-- {
		if t := strings.TrimSpace(lines[j]); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// isRule reports a horizontal rule. Claude Code may print the conversation's
// title inside the input box's top rule (`──── Fix the login ──`), so a rule
// starts and ends with a dash and is mostly dashes, not only dashes.
func isRule(s string) bool {
	r := []rune(strings.TrimSpace(s))
	if len(r) < 8 {
		return false
	}
	dash := func(c rune) bool { return strings.ContainsRune("─━╌┄-", c) }
	if !dash(r[0]) || !dash(r[len(r)-1]) {
		return false
	}
	n := 0
	for _, c := range r {
		if dash(c) {
			n++
		}
	}
	return n*2 >= len(r)
}

func indent(s string) int { return len(s) - len(strings.TrimLeft(s, " ")) }

func reverse(s []string) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[len(s)-1-i] = v
	}
	return out
}

func reverseOptions(s []DialogOption) []DialogOption {
	out := make([]DialogOption, len(s))
	for i, v := range s {
		out[len(s)-1-i] = v
	}
	return out
}

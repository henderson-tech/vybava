package transcripts

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
)

// Rollout response items that carry tool traffic. A custom_tool_call "exec"
// holds a JS program that calls tools (tools.exec_command, tools.apply_patch);
// a function_call holds JSON arguments. Outputs pair by call_id.
const (
	RolloutResponseItem = "response_item"
	ItemFunctionCall    = "function_call"
	ItemFunctionOutput  = "function_call_output"
	ItemCustomCall      = "custom_tool_call"
	ItemCustomOutput    = "custom_tool_call_output"
)

// ResponseItem is a response_item payload, decoded for tool traffic.
type ResponseItem struct {
	Type      string          `json:"type"`
	Name      string          `json:"name"`
	CallID    string          `json:"call_id"`
	Input     string          `json:"input"`
	Arguments string          `json:"arguments"`
	Output    json.RawMessage `json:"output"`
}

// RolloutToolLine is the cheap prefilter for tool calls and their outputs.
func RolloutToolLine(line []byte) bool {
	return bytes.Contains(line, []byte(`"function_call`)) || bytes.Contains(line, []byte(`"custom_tool_call`))
}

// OutputText is the text an output item returned into context: a plain
// string, or the input_text blocks of an array joined.
func (it ResponseItem) OutputText() string {
	var s string
	if json.Unmarshal(it.Output, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(it.Output, &parts) != nil {
		return ""
	}
	var texts []string
	for _, p := range parts {
		if p.Type == "input_text" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "")
}

// CodexCommand is one shell command a Codex tool call ran.
type CodexCommand struct {
	Cmd     string
	Workdir string
}

// Commands returns the shell commands a call item runs and the apply_patch
// bodies it sends. An exec program is read for string literals only: a
// command built at runtime from variables is invisible, never guessed.
func (it ResponseItem) Commands() (cmds []CodexCommand, patches []string) {
	switch {
	case it.Type == ItemCustomCall && it.Name == "exec":
		return parseExec(it.Input)
	case it.Type == ItemCustomCall && it.Name == "apply_patch":
		return nil, []string{it.Input}
	case it.Type == ItemFunctionCall && (it.Name == "exec_command" || it.Name == "shell"):
		var args struct {
			Cmd     json.RawMessage `json:"cmd"`
			Command []string        `json:"command"`
			Workdir string          `json:"workdir"`
		}
		if json.Unmarshal([]byte(it.Arguments), &args) != nil {
			return nil, nil
		}
		var cmd string
		if json.Unmarshal(args.Cmd, &cmd) != nil {
			var argv []string // an argv-shaped cmd
			if json.Unmarshal(args.Cmd, &argv) == nil {
				args.Command = argv
			}
		}
		if cmd == "" && len(args.Command) > 0 {
			cmd = argvCommand(args.Command)
		}
		if cmd != "" {
			cmds = append(cmds, CodexCommand{Cmd: cmd, Workdir: args.Workdir})
		}
	}
	return cmds, patches
}

// argvCommand renders an argv as the command line it runs: the script of a
// `bash -lc <script>` wrapper, else the words joined.
func argvCommand(argv []string) string {
	if n := len(argv); n >= 3 && strings.HasPrefix(argv[n-2], "-") && strings.Contains(argv[n-2], "c") {
		return argv[n-1]
	}
	return strings.Join(argv, " ")
}

var (
	execToolCall = regexp.MustCompile(`tools\.([A-Za-z0-9_]+)\s*\(`)
	execCmdKey   = regexp.MustCompile(`(?:^|[{,\s])cmd\s*:\s*`)
	execDirKey   = regexp.MustCompile(`(?:^|[{,\s])workdir\s*:\s*`)
	execPatchKey = regexp.MustCompile(`(?:^|[{,\s])patch\s*:\s*`)
)

// parseExec reads the tool calls of an exec program. Each tools.<name>( call
// in code — not inside a string or a comment — owns the source up to the next.
func parseExec(js string) (cmds []CodexCommand, patches []string) {
	literals := jsLiteralRanges(js)
	var calls [][]int
	for _, m := range execToolCall.FindAllStringSubmatchIndex(js, -1) {
		if !inRanges(m[0], literals) {
			calls = append(calls, m)
		}
	}
	for i, m := range calls {
		end := len(js)
		if i+1 < len(calls) {
			end = calls[i+1][0]
		}
		body := js[m[1]:end]
		switch js[m[2]:m[3]] {
		case "exec_command":
			if cmd, ok := jsKeyString(body, execCmdKey); ok {
				dir, _ := jsKeyString(body, execDirKey)
				cmds = append(cmds, CodexCommand{Cmd: cmd, Workdir: dir})
			}
		case "apply_patch":
			patch, ok := jsKeyString(body, execPatchKey)
			if !ok {
				patch, ok = jsString(strings.TrimLeft(body, " \t\n"))
			}
			if ok {
				patches = append(patches, patch)
			}
		}
	}
	return cmds, patches
}

// jsLiteralRanges lists the [start, end) byte ranges of a JS program's string
// literals and comments. A template's ${…} is part of its literal: code there
// is not read.
func jsLiteralRanges(js string) [][2]int {
	var out [][2]int
	var prev byte // the last code character before i, whitespace skipped
	for i := 0; i < len(js); i++ {
		end := -1
		switch c := js[i]; {
		case c == '/' && !strings.HasPrefix(js[i:], "//") && !strings.HasPrefix(js[i:], "/*") &&
			(prev == 0 || strings.IndexByte("(,=:[!&|?{};+-*%<>~^", prev) >= 0):
			end = regexEnd(js, i) // a `/` where a value starts opens a regex
		case c == '/' && strings.HasPrefix(js[i:], "//"):
			if end = strings.IndexByte(js[i:], '\n'); end < 0 {
				end = len(js)
			} else {
				end += i
			}
		case c == '/' && strings.HasPrefix(js[i:], "/*"):
			if end = strings.Index(js[i+2:], "*/"); end < 0 {
				end = len(js)
			} else {
				end += i + 4
			}
		case c == '\'' || c == '"' || c == '`':
			j := i + 1
			for j < len(js) && js[j] != c {
				if js[j] == '\\' {
					j++
				}
				j++
			}
			end = min(j+1, len(js))
		}
		if end >= 0 {
			out = append(out, [2]int{i, end})
			i = end - 1
			prev = 'x' // a literal is a value
		} else if !strings.ContainsRune(" \t\r\n", rune(js[i])) {
			prev = js[i]
		}
	}
	return out
}

// regexEnd returns the end of the regex literal opening at i (its flags
// included), or -1 when the line ends first: then the `/` was division.
func regexEnd(js string, i int) int {
	inClass := false
	for j := i + 1; j < len(js); j++ {
		switch js[j] {
		case '\\':
			j++
		case '\n':
			return -1
		case '[':
			inClass = true
		case ']':
			inClass = false
		case '/':
			if !inClass {
				for j++; j < len(js) && (js[j] >= 'a' && js[j] <= 'z'); j++ {
				}
				return j
			}
		}
	}
	return -1
}

func inRanges(pos int, ranges [][2]int) bool {
	for _, r := range ranges {
		if pos >= r[0] && pos < r[1] {
			return true
		}
	}
	return false
}

// jsKeyString decodes the string literal that follows the first key match.
func jsKeyString(s string, key *regexp.Regexp) (string, bool) {
	m := key.FindStringIndex(s)
	if m == nil {
		return "", false
	}
	return jsString(s[m[1]:])
}

// jsString decodes the JS string literal s starts with ('…', "…" or `…`).
// Escapes decode; a template's ${…} stays as written.
func jsString(s string) (string, bool) {
	if s == "" || !strings.ContainsRune("'\"`", rune(s[0])) {
		return "", false
	}
	q := s[0]
	var b strings.Builder
	for i := 1; i < len(s); i++ {
		c := s[i]
		switch {
		case c == q:
			return b.String(), true
		case c == '\\' && i+1 < len(s):
			i++
			switch e := s[i]; e {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case 'u':
				if i+4 < len(s) {
					if r, err := strconv.ParseUint(s[i+1:i+5], 16, 32); err == nil {
						b.WriteRune(rune(r))
						i += 4
						continue
					}
				}
				b.WriteByte(e)
			default:
				b.WriteByte(e)
			}
		default:
			b.WriteByte(c)
		}
	}
	return "", false
}

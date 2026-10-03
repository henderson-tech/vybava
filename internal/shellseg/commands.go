package shellseg

import "strings"

// Command is one local segment plus the NAME=value words the shell applies
// to it as environment. Segments() trims those words off, which is right
// for "what runs" and wrong for a rule that cares what the command is
// pointed at (`ANDROID_SERIAL=<serial> ./gradlew installRelease`).
type Command struct {
	Text   string
	Assign []string
}

// LocalCommands is LocalSegments keeping each segment's leading
// assignments (values unquoted). Same splitting, same payload recursion.
func LocalCommands(cmd string) []Command {
	return appendCommands(nil, cmd, 0)
}

func appendCommands(out []Command, cmd string, depth int) []Command {
	for _, p := range SplitScript(cmd) {
		raw := TrimSubshell(strings.Trim(p.Text, " \t\r"))
		s := TrimAssignments(raw)
		if s == "" || RemoteRunners[CommandWord(s)] {
			continue
		}
		var assign []string
		for _, f := range Fields(raw) {
			if !AssignPrefix.MatchString(f) {
				break
			}
			assign = append(assign, f)
		}
		out = append(out, Command{Text: s, Assign: assign})
		if depth >= MaxRunnerDepth {
			continue
		}
		for _, payload := range RunnerPayloads(s) {
			out = appendCommands(out, payload, depth+1)
		}
	}
	return out
}

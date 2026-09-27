package readeff

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/transcripts"
)

// readCodex builds a session from one Codex rollout. A call pairs with its
// output by call_id. An exec program running several commands returns one
// output, so its lines belong to the call as a whole: emptiness and search
// hits are judged only for a single-command call.
func readCodex(path string) (Session, error) {
	s := Session{Agent: "codex"}
	cwd := ""
	type pending struct {
		item transcripts.ResponseItem
		cwd  string
	}
	open := map[string]pending{}
	_, err := transcripts.Scan(path, transcripts.Cursor{}, false, wholeRead, func(line []byte, _ int64) error {
		meta := transcripts.RolloutUsageLine(line)
		if !meta && !transcripts.RolloutToolLine(line) && !bytes.Contains(line, []byte(`"compacted"`)) {
			return nil
		}
		var rl transcripts.RolloutLine
		if json.Unmarshal(line, &rl) != nil {
			return nil // a torn or foreign line carries no call we can pair
		}
		switch rl.Type {
		case transcripts.RolloutSessionMeta:
			var m transcripts.SessionMeta
			if json.Unmarshal(rl.Payload, &m) == nil && s.ID == "" {
				s.ID, cwd = m.ID, m.CWD
				s.Start, _ = time.Parse(time.RFC3339Nano, m.Timestamp)
				s.Repo, _ = transcripts.GitRoot(m.CWD)
			}
		case transcripts.RolloutTurnContext:
			var tc transcripts.TurnContext
			if json.Unmarshal(rl.Payload, &tc) == nil && tc.CWD != "" {
				cwd = tc.CWD
			}
		case "compacted":
			s.Calls = append(s.Calls, Call{Tool: compactTool, Class: ClassOther})
		case transcripts.RolloutResponseItem:
			var it transcripts.ResponseItem
			if json.Unmarshal(rl.Payload, &it) != nil {
				return nil
			}
			switch it.Type {
			case transcripts.ItemFunctionCall, transcripts.ItemCustomCall:
				open[it.CallID] = pending{item: it, cwd: cwd}
			case transcripts.ItemFunctionOutput, transcripts.ItemCustomOutput:
				if p, ok := open[it.CallID]; ok {
					delete(open, it.CallID)
					s.Calls = append(s.Calls, codexCall(p.item, p.cwd, execOutput(it.OutputText())))
				}
			}
		}
		return nil
	})
	return s, err
}

// execOutput drops the header an exec program's output opens with
// ("Script completed", wall time, "Output:"): it is the harness talking.
func execOutput(s string) string {
	if !strings.HasPrefix(s, "Script ") && !strings.HasPrefix(s, "Wall time") {
		return s
	}
	if i := strings.Index(s, "Output:\n"); i >= 0 {
		return s[i+len("Output:\n"):]
	}
	return s
}

// codexCall reduces one paired call and output.
func codexCall(it transcripts.ResponseItem, cwd, out string) Call {
	cmds, patches := it.Commands()
	c := Call{Tool: it.Name, Class: ClassOther, Lines: countLines(out)}
	var read, search, edit bool
	for _, cmd := range cmds {
		dir := firstNonEmpty(cmd.Workdir, cwd)
		sc := shellCall(cmd.Cmd, dir)
		read = read || sc.Class == ClassRead
		search = search || sc.Class == ClassSearch
		edit = edit || sc.Class == ClassEdit
		c.Spans = append(c.Spans, sc.Spans...)
		c.Edited = append(c.Edited, sc.Edited...)
		if c.Query == "" && sc.Query != "" {
			c.Query, c.dir = sc.Query, sc.dir
		}
	}
	for _, p := range patches {
		files, changed := patchChanged(p, cwd)
		edit, c.Edited, c.Changed = true, append(c.Edited, files...), c.Changed+changed
	}
	switch {
	case edit:
		c.Class = ClassEdit
	case read:
		c.Class = ClassRead
	case search:
		c.Class = ClassSearch
	}
	if len(cmds) == 1 && len(patches) == 0 {
		c = settle(c, out, false)
		if c.Class == ClassSearch && !c.Empty {
			c.Found = foundPaths(out, c.dir)
		}
	} else if c.Class == ClassSearch {
		c.Query = "" // several commands share one output: no retry to judge
	}
	return c
}

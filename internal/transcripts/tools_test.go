package transcripts

import (
	"slices"
	"testing"
)

// A Claude tool call and its result pair by id; the result's structured
// outcome says what a Read returned.
func TestClaudeToolTraffic(t *testing.T) {
	call, err := DecodeClaudeTool([]byte(`{"type":"assistant","message":{"content":[{"type":"text","text":"x"},{"type":"tool_use","id":"t1","name":"Read","input":{"file_path":"/r/a.go"}}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	uses := call.Message.ToolUses()
	if len(uses) != 1 || uses[0].ID != "t1" || uses[0].Name != "Read" {
		t.Fatalf("ToolUses = %+v", uses)
	}
	res, err := DecodeClaudeTool([]byte(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"a\nb"}]}]},"toolUseResult":{"type":"text","file":{"filePath":"/r/a.go","startLine":1,"numLines":2,"totalLines":90}}}`))
	if err != nil {
		t.Fatal(err)
	}
	results := res.Message.ToolResults()
	if len(results) != 1 || results[0].UseID != "t1" || results[0].Text != "a\nb" {
		t.Fatalf("ToolResults = %+v", results)
	}
	out, ok := res.Outcome()
	if !ok || out.File == nil || out.File.TotalLines != 90 || out.File.NumLines != 2 {
		t.Fatalf("Outcome = %+v, %v", out, ok)
	}
	failed := ClaudeToolRecord{ToolUseResult: []byte(`"Error: Exit code 1"`)}
	if _, ok := failed.Outcome(); ok {
		t.Error("a string toolUseResult decoded as an outcome")
	}
}

// An exec program's literal commands and patches are read; a command built
// from a variable is not guessed.
func TestCodexExecCommands(t *testing.T) {
	js := "// tools.exec_command({cmd:\"cat commented.go\"})\n" +
		"const note = \"tools.exec_command({cmd:'cat quoted.go'})\";\n" +
		"const p = /tools.exec_command({cmd:\"cat regex.go\"})/g;\n" +
		"const r = await Promise.allSettled([\n" +
		`  tools.exec_command({cmd:"sed -n '1,40p' a.go && echo \"done\"",workdir:"/repo",max_output_tokens:3000}),` + "\n" +
		"  tools.exec_command({cmd:`rg -n foo`}),\n" +
		"  tools.exec_command({cmd: built}),\n" +
		"  tools.exec_command({cmd:`cat ${f}`}),\n" +
		"  tools.apply_patch(`*** Begin Patch\n*** Update File: a.go\n-x\n+y\n*** End Patch`),\n" +
		"]);"
	cmds, patches := ResponseItem{Type: ItemCustomCall, Name: "exec", Input: js}.Commands()
	want := []CodexCommand{{Cmd: `sed -n '1,40p' a.go && echo "done"`, Workdir: "/repo"}, {Cmd: "rg -n foo"}}
	if !slices.Equal(cmds, want) {
		t.Errorf("commands = %q, want %q", cmds, want)
	}
	if len(patches) != 1 || patches[0] != "*** Begin Patch\n*** Update File: a.go\n-x\n+y\n*** End Patch" {
		t.Errorf("patches = %q", patches)
	}
}

// Older rollouts call the shell as a function with JSON arguments, the
// command as a string or as a `bash -lc` argv.
func TestCodexFunctionCallCommands(t *testing.T) {
	for args, want := range map[string]string{
		`{"cmd":"cat a.go","workdir":"/r"}`:           "cat a.go",
		`{"command":["bash","-lc","grep -n x a.go"]}`: "grep -n x a.go",
		`{"cmd":["bash","-lc","ls"],"workdir":"/r"}`:  "ls",
	} {
		cmds, _ := ResponseItem{Type: ItemFunctionCall, Name: "exec_command", Arguments: args}.Commands()
		if len(cmds) != 1 || cmds[0].Cmd != want {
			t.Errorf("%s → %q, want %q", args, cmds, want)
		}
	}
	out := ResponseItem{Output: []byte(`[{"type":"input_text","text":"Script completed\nOutput:\n"},{"type":"input_image"},{"type":"input_text","text":"a\nb\n"}]`)}
	if got := out.OutputText(); got != "Script completed\nOutput:\na\nb\n" {
		t.Errorf("OutputText = %q", got)
	}
}

package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/henderson-tech/vybava/internal/runx"
)

func runLok(t *testing.T, dir string, args ...string) (runx.Envelope, string, error) {
	t.Helper()
	t.Chdir(dir)
	var out, errOut bytes.Buffer
	command, err := (App{Stdout: &out, Stderr: &errOut}).Command("lok")
	if err != nil {
		t.Fatal(err)
	}
	command.SetArgs(args)
	err = command.Execute()
	var env runx.Envelope
	if contains(args, "--json") {
		if jerr := json.Unmarshal(out.Bytes(), &env); jerr != nil {
			t.Fatalf("%v: stdout is not one envelope: %q (%v)", args, out.String(), jerr)
		}
	}
	return env, out.String(), err
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// The dry run's next line IS the write that applies exactly the reviewed
// count; the human diff shows invisible runes; rm --locale is wired.
func TestLokSubNextIsTheReviewedWrite(t *testing.T) {
	dir := t.TempDir()
	for rel, body := range map[string]string{
		"vybava.config.json": `{"lok":{"catalogs":{"dict":{"style":"path","files":"dict/{locale}.json","locales":["en","cs"]}}}}`,
		"dict/en.json":       "{\n  \"codes\": {\n    \"bankid.x\": \"Wait \u2014 now\"\n  }\n}\n",
		"dict/cs.json":       "{\n  \"codes\": {\n    \"bankid.x\": \"Počkej \u2014 teď\",\n    \"only\": \"cs\"\n  }\n}\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_, text, err := runLok(t, dir, "sub", ` ?\x{2014} ?`, " - ", "--catalog=dict")
	if err != nil || !strings.Contains(text, `codes.bankid\.x`) || !strings.Contains(text, `- Wait \x{2014} now`) || !strings.Contains(text, "2 values in 1 keys") {
		t.Fatalf("human dry run: %v\n%s", err, text)
	}
	env, _, err := runLok(t, dir, "sub", ` ?\x{2014} ?`, " - ", "--catalog=dict", "--json")
	want := `lok sub ' ?\x{2014} ?' ' - ' --catalog=dict --write --expect 2 --json`
	if err != nil || !env.OK || len(env.Next) != 1 || env.Next[0] != want {
		t.Fatalf("next must be the reviewed write: %v %+v", err, env.Next)
	}
	env, _, err = runLok(t, dir, "sub", ` ?\x{2014} ?`, " - ", "--catalog=dict", "--write", "--expect", "2", "--json")
	if err != nil || !env.OK || strings.Join(env.Next, "|") != "lok check --catalog=dict --json" {
		t.Fatalf("write: %v %+v", err, env)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "dict/cs.json")); !strings.Contains(string(b), `"Počkej - teď"`) {
		t.Fatalf("written:\n%s", b)
	}
	env, _, err = runLok(t, dir, "rm", "codes.only", "--locale", "cs", "--json")
	if err != nil || !env.OK {
		t.Fatalf("rm --locale: %v %+v", err, env)
	}
}

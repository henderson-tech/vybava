package tsgate

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestStripJSONC(t *testing.T) {
	src := `{
  // a line comment
  "a": "keeps // and /* inside strings", /* a block */
  "b": "escaped \" quote",
  "c": [1, 2, /* trailing */ ],
  "d": 1, // trailing comma before the brace
}`
	var got map[string]any
	if err := json.Unmarshal(stripJSONC([]byte(src)), &got); err != nil {
		t.Fatalf("%v\n%s", err, stripJSONC([]byte(src)))
	}
	want := map[string]any{"a": "keeps // and /* inside strings", "b": `escaped " quote`, "c": []any{1.0, 2.0}, "d": 1.0}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// repo lays out a checkout with a node10 + baseUrl base config (FixIt's api
// shape), an Expo-like client extending two packages, and clean packages.
func repo(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "tsconfig.base.json"), `{
  // shared by every app
  "compilerOptions": {
    "baseUrl": ".",
    "moduleResolution": "node",
    "module": "commonjs",
    "paths": { "@app/*": ["packages/app/src/*"] },
    "incremental": true,
    "ignoreDeprecations": "6.0",
    "strict": true,
  },
}`)
	write(t, filepath.Join(root, "apps/api/tsconfig.json"), `{
  "extends": "../../tsconfig.base.json",
  "compilerOptions": { "outDir": "./dist", "typeRoots": ["./types"] },
  "include": ["src/**/*", "test/**/*"]
}`)
	write(t, filepath.Join(root, "node_modules/expo/package.json"), `{"name":"expo","exports":{"./tsconfig.base":"./tsconfig.base.json"}}`)
	write(t, filepath.Join(root, "node_modules/expo/tsconfig.base.json"), `{"compilerOptions":{"module":"preserve","moduleResolution":"bundler","rootDir":"${configDir}","outDir":"${configDir}/.out"}}`)
	write(t, filepath.Join(root, "node_modules/@tsconfig/strictest/package.json"), `{"name":"@tsconfig/strictest"}`)
	write(t, filepath.Join(root, "node_modules/@tsconfig/strictest/tsconfig.json"), `{"compilerOptions":{"noUncheckedIndexedAccess":true}}`)
	write(t, filepath.Join(root, "apps/client/tsconfig.json"), `{
  "extends": ["expo/tsconfig.base", "@tsconfig/strictest"],
  "compilerOptions": { "baseUrl": ".", "paths": { "@/*": ["./*"] } },
  "include": ["**/*.ts"]
}`)
	write(t, filepath.Join(root, "packages/lib/tsconfig.json"), `{"compilerOptions":{"module":"esnext","moduleResolution":"bundler","strict":true}}`)
	write(t, filepath.Join(root, "packages/reset/tsconfig.json"), `{"extends":"../../tsconfig.base.json","compilerOptions":{"baseUrl":null,"paths":null,"module":"esnext","moduleResolution":"bundler"}}`)
	return root
}

func options(t *testing.T, d *Derived) map[string]any {
	t.Helper()
	raw, err := json.Marshal(d.CompilerOptions)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestPlanDerivesTheNode10Chain(t *testing.T) {
	root := repo(t)
	plan, err := PlanProgram(filepath.Join(root, "apps/api/tsconfig.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Derive || len(plan.Reasons) != 3 || plan.Target != filepath.Join(root, "apps/api/.tsconfig.ts7.json") {
		t.Fatalf("plan = %+v", plan)
	}
	got := options(t, plan.Config)
	want := map[string]any{
		"strict":                    true,
		"module":                    "preserve",
		"moduleResolution":          "bundler",
		"resolvePackageJsonExports": false,
		"outDir":                    "./dist",
		"typeRoots":                 []any{"./types"},
		"rootDir":                   "../..",
		"paths":                     map[string]any{"@app/*": []any{"../../packages/app/src/*"}, "*": []any{"../../*"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("compilerOptions\n got %v\nwant %v", got, want)
	}
	if plan.Config.Include == nil || !reflect.DeepEqual(*plan.Config.Include, []string{"./src/**/*", "./test/**/*"}) {
		t.Errorf("include = %v", plan.Config.Include)
	}
}

func TestPlanResolvesPackagesAndConfigDir(t *testing.T) {
	root := repo(t)
	plan, err := PlanProgram(filepath.Join(root, "apps/client/tsconfig.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Derive || len(plan.Reasons) != 1 {
		t.Fatalf("only baseUrl should derive: %+v", plan)
	}
	got := options(t, plan.Config)
	want := map[string]any{
		"module":                   "preserve", // not node10: never forced
		"moduleResolution":         "bundler",
		"rootDir":                  ".", // ${configDir} is the leaf's directory
		"outDir":                   "./.out",
		"noUncheckedIndexedAccess": true,
		"paths":                    map[string]any{"@/*": []any{"./*"}, "*": []any{"./*"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("compilerOptions\n got %v\nwant %v", got, want)
	}
}

func TestPlanKeepsReferencesAndScopesRootDir(t *testing.T) {
	root := repo(t)
	// Derived for baseUrl/node10 only: no outDir, so no rootDir is invented,
	// and the leaf's references ride along verbatim.
	write(t, filepath.Join(root, "packages/refs/tsconfig.json"), `{"extends":"../../tsconfig.base.json","references":[{"path":"../lib"}]}`)
	plan, err := PlanProgram(filepath.Join(root, "packages/refs/tsconfig.json"))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := plan.Config.CompilerOptions["rootDir"]; !plan.Derive || ok {
		t.Errorf("rootDir without outDir: %+v", plan.Config.CompilerOptions)
	}
	if string(plan.Config.References) != `[{"path":"../lib"}]` {
		t.Errorf("references = %s", plan.Config.References)
	}
	// Outside every checkout, outDir without rootDir takes the volume root.
	loose := t.TempDir()
	write(t, filepath.Join(loose, "tsconfig.json"), `{"compilerOptions":{"moduleResolution":"bundler","outDir":"out"}}`)
	plan, err = PlanProgram(filepath.Join(loose, "tsconfig.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rootDir string
	if !plan.Derive || json.Unmarshal(plan.Config.CompilerOptions["rootDir"], &rootDir) != nil || filepath.Clean(filepath.Join(loose, rootDir)) != string(filepath.Separator) {
		t.Errorf("rootDir outside a checkout = %q (%+v)", rootDir, plan)
	}
}

func TestWriteDerivedIsSafeForConcurrentRuns(t *testing.T) {
	root := repo(t)
	plan, err := PlanProgram(filepath.Join(root, "apps/api/tsconfig.json"))
	if err != nil {
		t.Fatal(err)
	}
	errs := make(chan error, 8)
	for range 8 {
		go func() { errs <- writeDerived(plan) }()
	}
	for range 8 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	if leftovers, _ := filepath.Glob(plan.Target + ".*.tmp"); len(leftovers) != 0 {
		t.Errorf("temp files left: %v", leftovers)
	}
}

func TestPlanLeavesAReadableChainAlone(t *testing.T) {
	root := repo(t)
	for _, p := range []string{"packages/lib/tsconfig.json", "packages/reset/tsconfig.json"} {
		plan, err := PlanProgram(filepath.Join(root, p))
		if err != nil {
			t.Fatal(err)
		}
		if plan.Derive || plan.Target != plan.Tsconfig || plan.Config != nil {
			t.Errorf("%s: %+v", p, plan)
		}
	}
	write(t, filepath.Join(root, "bad/tsconfig.json"), `{"extends":"missing-pkg/tsconfig"}`)
	if _, err := PlanProgram(filepath.Join(root, "bad/tsconfig.json")); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Errorf("missing extends: %v", err)
	}
}

// ts7 installs a fake TS 7 whose native binary runs script, the way bun's
// isolated store lays it out: the alias symlinks into a store directory that
// holds the platform package beside it.
func ts7(t *testing.T, project, script string) string {
	t.Helper()
	store := filepath.Join(t.TempDir(), "store", "node_modules")
	write(t, filepath.Join(store, "typescript/package.json"), `{"name":"typescript","version":"7.0.2","bin":{"tsc":"./bin/tsc"}}`)
	exe := filepath.Join(store, "@typescript", "typescript-"+nodePlatform()+"-"+nodeArch(), "lib", "tsc")
	write(t, exe, "#!/bin/sh\n"+script)
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	if err := os.MkdirAll(filepath.Join(project, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(store, "typescript"), filepath.Join(project, "node_modules", "typescript7")); err != nil {
		t.Fatal(err)
	}
	return exe
}

func TestFindTS7(t *testing.T) {
	root := repo(t)
	api := filepath.Join(root, "apps/api")
	write(t, filepath.Join(root, "node_modules/typescript/package.json"), `{"name":"typescript","version":"6.0.3","bin":{"tsc":"./bin/tsc"}}`)
	if _, err := FindTS7(api, nil); !errors.Is(err, errNotFound) || !strings.Contains(err.Error(), "typescript 6.0.3") || !strings.Contains(err.Error(), `"typescript7"`) {
		t.Fatalf("only TS 6 installed: %v", err)
	}
	exe := ts7(t, api, "exit 0\n")
	c, err := FindTS7(api, nil)
	if err != nil || c.Dependency != "typescript7" || c.Version != "7.0.2" || c.Command[0] != exe {
		t.Fatalf("isolated layout = %+v, %v", c, err)
	}
	// Hoisted: the alias and the platform package side by side at the root.
	hoisted := repo(t)
	write(t, filepath.Join(hoisted, "node_modules/typescript7/package.json"), `{"name":"typescript","version":"7.0.2"}`)
	hexe := filepath.Join(hoisted, "node_modules/@typescript/typescript-"+nodePlatform()+"-"+nodeArch()+"/lib/tsc")
	write(t, hexe, "#!/bin/sh\n")
	if real, err := filepath.EvalSymlinks(hexe); err == nil {
		hexe = real
	}
	if c, err := FindTS7(filepath.Join(hoisted, "apps/api"), nil); err != nil || c.Command[0] != hexe {
		t.Fatalf("hoisted layout = %+v, %v", c, err)
	}
	if err := os.Remove(hexe); err != nil {
		t.Fatal(err)
	}
	if _, err := FindTS7(filepath.Join(hoisted, "apps/api"), nil); err == nil || !strings.Contains(err.Error(), "platform package") {
		t.Errorf("missing platform package: %v", err)
	}
}

func TestRunExecsTS7OnTheDerivedConfig(t *testing.T) {
	root := repo(t)
	api := filepath.Join(root, "apps/api")
	argsFile := filepath.Join(t.TempDir(), "args")
	ts7(t, api, "echo \"$@\" > '"+argsFile+"'\necho 'src/a.ts(1,1): error TS2322: nope'\nexit 2\n")
	var out bytes.Buffer
	code, err := Run(filepath.Join(api, "tsconfig.json"), Options{Args: []string{"--extendedDiagnostics"}, Stdout: &out, Stderr: &out})
	if err != nil || code != 2 || !strings.Contains(out.String(), "TS2322") {
		t.Fatalf("code %d, err %v, out %q", code, err, out.String())
	}
	derived := filepath.Join(api, ".tsconfig.ts7.json")
	args, _ := os.ReadFile(argsFile)
	if got := strings.TrimSpace(string(args)); got != "-p "+derived+" --noEmit --extendedDiagnostics" {
		t.Errorf("argv = %q", got)
	}
	var d Derived
	raw, err := os.ReadFile(derived)
	if err != nil || json.Unmarshal(raw, &d) != nil || !strings.Contains(d.Comment, "tsconfig.json") {
		t.Fatalf("derived config: %v %s", err, raw)
	}
	// -b builds the real tsconfig, so a chain that needs deriving is refused.
	if _, err := Run(filepath.Join(api, "tsconfig.json"), Options{Build: true, Stdout: &out, Stderr: &out}); err == nil || !strings.Contains(err.Error(), "baseUrl") {
		t.Errorf("build on a derived chain: %v", err)
	}
	lib := filepath.Join(root, "packages/lib")
	ts7(t, lib, "echo \"$@\" > '"+argsFile+"'\n")
	if code, err := Run(filepath.Join(lib, "tsconfig.json"), Options{Build: true, Stdout: &out, Stderr: &out}); err != nil || code != 0 {
		t.Fatalf("build: %d %v", code, err)
	}
	args, _ = os.ReadFile(argsFile)
	if got := strings.TrimSpace(string(args)); got != "-b "+filepath.Join(lib, "tsconfig.json") {
		t.Errorf("build argv = %q", got)
	}
}

func TestParity(t *testing.T) {
	root := repo(t)
	api := filepath.Join(root, "apps/api")
	shared := "echo 'src/a.ts(1,5): error TS2322: Type number is not assignable'\necho '  to type string.'\n"
	ts7(t, api, shared+"exit 2\n")
	// The classic compiler is JavaScript run by node; a fake node runs it as sh.
	write(t, filepath.Join(root, "node_modules/typescript/package.json"), `{"name":"typescript","version":"6.0.3","bin":{"tsc":"./bin/tsc"}}`)
	write(t, filepath.Join(root, "node_modules/typescript/bin/tsc"), shared+"echo 'error TS5102: baseUrl'\nexit 2\n")
	bin := t.TempDir()
	write(t, filepath.Join(bin, "node"), "#!/bin/sh\nexec sh \"$@\"\n")
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	r, err := Parity(filepath.Join(api, "tsconfig.json"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Same || r.Baseline.Diagnostics != 2 || r.TS7.Diagnostics != 1 || len(r.OnlyTS7) != 0 {
		t.Fatalf("report = %+v", r)
	}
	if len(r.OnlyBaseline) != 1 || r.OnlyBaseline[0].Code != "TS5102" || r.Baseline.Compiler.Version != "6.0.3" || r.TS7.Compiler.Version != "7.0.2" {
		t.Errorf("onlyBaseline = %+v", r.OnlyBaseline)
	}

	// A side that fails without a diagnostic is inconclusive, never parity.
	lib := filepath.Join(root, "packages/lib")
	ts7(t, lib, "echo 'Usage: tsc [options]'\nexit 1\n")
	write(t, filepath.Join(root, "node_modules/typescript/bin/tsc"), "exit 0\n")
	if _, err := Parity(filepath.Join(lib, "tsconfig.json"), Options{}); err == nil || !strings.Contains(err.Error(), "inconclusive") || !strings.Contains(err.Error(), "Usage: tsc") {
		t.Errorf("crashed side: %v", err)
	}
}

func TestCheckReturnsTheResultAsData(t *testing.T) {
	root := repo(t)
	lib := filepath.Join(root, "packages/lib")
	ts7(t, lib, "echo 'src/a.ts(2,3): error TS2304: Cannot find name x.'\nexit 2\n")
	r, err := Check(filepath.Join(lib, "tsconfig.json"), Options{})
	if err != nil || r.Exit != 2 || r.Derive || len(r.Diagnostics) != 1 || r.Diagnostics[0].Code != "TS2304" || r.Output != "" || r.Compiler.Version != "7.0.2" {
		t.Fatalf("check = %+v, %v", r, err)
	}
	ts7(t, filepath.Join(root, "packages/reset"), "echo 'panic: boom'\nexit 3\n")
	if r, err := Check(filepath.Join(root, "packages/reset/tsconfig.json"), Options{}); err != nil || r.Exit != 3 || len(r.Diagnostics) != 0 || !strings.Contains(r.Output, "panic: boom") {
		t.Errorf("unexplained exit keeps its output: %+v, %v", r, err)
	}
}

func TestParseAndDiffDiagnostics(t *testing.T) {
	out := "src/a.ts(3,7): error TS2322: Type 'number' is not assignable to type 'string'.\n" +
		"  The expected type comes from here.\n" +
		"error TS5102: Option 'baseUrl' has been removed.\n" +
		"src/a.ts(3,7): error TS2322: again\n" +
		"\nFound 3 errors.\n"
	diags := parseDiagnostics(out)
	if len(diags) != 3 || diags[0].Line != 3 || diags[0].Col != 7 || !strings.Contains(diags[0].Message, "\nThe expected type") || diags[1].File != "" {
		t.Fatalf("parsed %+v", diags)
	}
	// Multisets: the same error twice on one side and once on the other differs.
	onlyA, onlyB := diffDiagnostics(diags, diags[:2])
	if len(onlyA) != 1 || onlyA[0].Message != "again" || len(onlyB) != 0 {
		t.Errorf("diff = %+v / %+v", onlyA, onlyB)
	}
}

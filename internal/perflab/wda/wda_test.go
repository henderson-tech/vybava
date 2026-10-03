package wda

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henderson-tech/vybava/internal/perflab/hostexec"
	"github.com/henderson-tech/vybava/internal/runx"
)

// fakeRun replays recorded command output; build creates the products a
// real build-for-testing leaves in -derivedDataPath.
type fakeRun struct {
	calls []string
	fn    func(c hostexec.Cmd) (hostexec.Result, error)
}

func (f *fakeRun) Run(_ context.Context, c hostexec.Cmd) (hostexec.Result, error) {
	f.calls = append(f.calls, strings.Join(c.Argv, " "))
	return f.fn(c)
}

func (f *fakeRun) ran(prefix string) int {
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// appiumHome lays out the driver tree the way `appium driver install
// xcuitest` does (versions from FixIt's appium/.appium-home, 2026-10-02).
func appiumHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	driver := filepath.Join(home, "node_modules", "appium-xcuitest-driver")
	wdaDir := filepath.Join(driver, "node_modules", "appium-webdriveragent")
	must(t, os.MkdirAll(filepath.Join(wdaDir, "WebDriverAgent.xcodeproj"), 0o755))
	must(t, os.WriteFile(filepath.Join(driver, "package.json"), []byte(`{"name":"appium-xcuitest-driver","version":"11.11.2"}`), 0o644))
	must(t, os.WriteFile(filepath.Join(wdaDir, "package.json"), []byte(`{"name":"appium-webdriveragent","version":"14.1.1"}`), 0o644))
	return home
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// writeProducts makes a derived data dir like the calendar campaign's
// /tmp/calendar-perf/wda-dd (iphoneos26.5 SDK).
func writeProducts(t *testing.T, derived string) {
	t.Helper()
	products := filepath.Join(derived, "Build", "Products")
	must(t, os.MkdirAll(filepath.Join(products, "Debug-iphoneos", runnerApp), 0o755))
	must(t, os.WriteFile(filepath.Join(products, "WebDriverAgentRunner_iphoneos26.5-arm64.xctestrun"), []byte("<plist/>"), 0o644))
	must(t, os.WriteFile(filepath.Join(products, "Debug-iphoneos", runnerApp, "Info.plist"), []byte("binary"), 0o644))
}

func diagCode(t *testing.T, err error) string {
	t.Helper()
	var d runx.DiagError
	if !errors.As(err, &d) {
		t.Fatalf("want a DiagError, got %v", err)
	}
	return d.Diag.Code
}

func newEnv(t *testing.T, run *fakeRun) Env {
	t.Helper()
	return Env{Run: run, Now: func() time.Time { return time.Date(2026, 10, 2, 18, 0, 0, 0, time.UTC) }, PipeCapacity: func() (int, error) { return 65536, nil }}
}

func TestParseXcodeVersion(t *testing.T) {
	v, b := parseXcodeVersion(string(readFixture(t, "xcodebuild-version.txt")))
	if v != "26.5" || b != "17F42" {
		t.Fatalf("got %q %q", v, b)
	}
}

func TestDefaultBundleID(t *testing.T) {
	for in, want := range map[string]string{
		"app.fixit.client": "app.fixit.WebDriverAgentRunner",
		"single":           "single.WebDriverAgentRunner",
		"":                 "",
	} {
		if got := DefaultBundleID(in); got != want {
			t.Errorf("DefaultBundleID(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestKeyChangesWithEveryInput(t *testing.T) {
	tc := Toolchain{XcodeBuild: "17F42", DriverVersion: "11.11.2", WDAVersion: "14.1.1"}
	s := Spec{Team: "YJ77YV2PNA", BundleID: "app.fixit.WebDriverAgentRunner"}
	base := Key(tc, s)
	if !strings.HasPrefix(base, "wda1-") || len(base) != len("wda1-")+16 {
		t.Fatalf("key shape %q", base)
	}
	variants := map[string]string{}
	x := tc
	x.XcodeBuild = "17F41" // the 2026-10-02 Xcode update that orphaned the campaign's WDA
	variants["xcode"] = Key(x, s)
	d := tc
	d.DriverVersion = "11.12.0"
	variants["driver"] = Key(d, s)
	w := tc
	w.WDAVersion = "14.2.0"
	variants["wda"] = Key(w, s)
	t2 := s
	t2.Team = "OTHERTEAM1"
	variants["team"] = Key(tc, t2)
	b2 := s
	b2.BundleID = "com.example.WebDriverAgentRunner"
	variants["bundle"] = Key(tc, b2)
	for name, k := range variants {
		if k == base {
			t.Errorf("changing %s kept the key %s", name, k)
		}
	}
	if Key(tc, s) != base {
		t.Fatal("key is not deterministic")
	}
}

func TestBuildMissThenHit(t *testing.T) {
	home := appiumHome(t)
	spec := Spec{AppiumHome: home, Team: "YJ77YV2PNA", BundleID: "app.fixit.WebDriverAgentRunner", CacheDir: t.TempDir()}
	run := &fakeRun{fn: func(c hostexec.Cmd) (hostexec.Result, error) {
		switch {
		case c.Argv[0] == "xcodebuild" && c.Argv[1] == "-version":
			return hostexec.Result{Stdout: readFixture(t, "xcodebuild-version.txt")}, nil
		case c.Argv[0] == "xcodebuild" && c.Argv[1] == "build-for-testing":
			derived := c.Argv[indexOf(c.Argv, "-derivedDataPath")+1]
			writeProducts(t, derived)
			_, _ = c.Log.Write([]byte("** TEST BUILD SUCCEEDED **\n"))
			return hostexec.Result{Duration: 95 * time.Second}, nil
		case c.Argv[0] == "plutil":
			return hostexec.Result{Stdout: []byte("app.fixit.WebDriverAgentRunner.xctrunner\n")}, nil
		}
		t.Fatalf("unexpected command %v", c.Argv)
		return hostexec.Result{}, nil
	}}
	env := newEnv(t, run)
	locked := 0
	env.HostLock = func(context.Context, string) (func(), error) { locked++; return func() {}, nil }

	if _, err := Find(context.Background(), env, spec); diagCode(t, err) != DiagWDAMissing {
		t.Fatal("find before build must be WDA_MISSING")
	}
	res, err := Build(context.Background(), env, spec, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	data := res.Data.(Data)
	if data.Hit {
		t.Fatal("first build reported a hit")
	}
	if locked != 1 {
		t.Fatalf("host lock taken %d times, want 1", locked)
	}
	argv := run.calls[indexOfPrefix(run.calls, "xcodebuild build-for-testing")]
	for _, want := range []string{"-scheme WebDriverAgentRunner", "-destination generic/platform=iOS", "DEVELOPMENT_TEAM=YJ77YV2PNA", "PRODUCT_BUNDLE_IDENTIFIER=app.fixit.WebDriverAgentRunner", "-allowProvisioningUpdates"} {
		if !strings.Contains(argv, want) {
			t.Errorf("build argv lacks %q: %s", want, argv)
		}
	}
	e := data.Entry
	if filepath.Base(e.DerivedDataPath) != e.Key || !strings.HasSuffix(e.XCTestRun, "WebDriverAgentRunner_iphoneos26.5-arm64.xctestrun") {
		t.Fatalf("entry paths %+v", e)
	}
	if _, err := os.Stat(e.XCTestRun); err != nil {
		t.Fatalf("xctestrun not at the final path: %v", err)
	}
	if data.Tokens["wdaDerivedData"] != e.DerivedDataPath || data.Tokens["wdaBundleId"] != "app.fixit.WebDriverAgentRunner" {
		t.Fatalf("tokens %v", data.Tokens)
	}
	if e.Toolchain.XcodeBuild != "17F42" || e.Toolchain.DriverVersion != "11.11.2" || e.Toolchain.WDAVersion != "14.1.1" || e.RunnerBundleID != "app.fixit.WebDriverAgentRunner.xctrunner" {
		t.Fatalf("manifest %+v", e)
	}
	if b, _ := os.ReadFile(e.BuildLog); !strings.Contains(string(b), "TEST BUILD SUCCEEDED") {
		t.Fatalf("build log not kept at %s", e.BuildLog)
	}
	leftovers, _ := filepath.Glob(filepath.Join(spec.CacheDir, "wda", ".tmp-*"))
	if len(leftovers) != 0 {
		t.Fatalf("temp dirs left behind: %v", leftovers)
	}

	again, err := Build(context.Background(), env, spec, BuildOptions{})
	if err != nil || !again.Data.(Data).Hit {
		t.Fatalf("second build must be a hit: %v %+v", err, again.Data)
	}
	if n := run.ran("xcodebuild build-for-testing"); n != 1 {
		t.Fatalf("xcodebuild ran %d times, want 1", n)
	}
	found, err := Find(context.Background(), env, spec)
	if err != nil || found.Data.(Data).Entry.Key != e.Key {
		t.Fatalf("find after build: %v", err)
	}
	list, err := List(spec)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %v", list, err)
	}
	// A deleted product reads as missing again.
	must(t, os.Remove(e.XCTestRun))
	if _, err := Find(context.Background(), env, spec); diagCode(t, err) != DiagWDAMissing {
		t.Fatal("a deleted xctestrun must read as WDA_MISSING")
	}
}

func TestBuildFailures(t *testing.T) {
	cases := []struct {
		name     string
		pipe     int
		build    hostexec.Result
		buildErr error
		want     string
		fix      string
	}{
		{name: "pipe memory exhausted refuses before xcodebuild", pipe: 512, want: DiagPipeCapacityLow, fix: "perflab doctor --for build"},
		{name: "no output for the stall window", pipe: 65536, build: hostexec.Result{Stalled: true, Exit: -1}, want: DiagBuildStalled, fix: "pipe probe"},
		{name: "signing", pipe: 65536, build: hostexec.Result{Exit: 65, Stdout: []byte("error: No Account for Team \"YJ77YV2PNA\". Add a new account in Accounts settings or verify that your accounts have valid credentials. (in target 'WebDriverAgentRunner' from project 'WebDriverAgent')\n** TEST BUILD FAILED **\n")}, want: DiagBuildFailed, fix: "Xcode > Settings > Accounts"},
		{name: "compile error", pipe: 65536, build: hostexec.Result{Exit: 65, Stdout: []byte("** TEST BUILD FAILED **\n")}, want: DiagBuildFailed, fix: "tail -n 80"},
		// The 2026-10-02 live run: Xcode 17F42 without its iOS 26.5 platform.
		{name: "iOS platform not downloaded", pipe: 65536, build: hostexec.Result{Exit: 70, Stdout: mustRead("testdata/xcodebuild-platform-missing.log")}, want: DiagToolMissing, fix: "xcodebuild -downloadPlatform iOS, then perflab wda build"},
		{name: "xcodebuild missing", pipe: 65536, buildErr: exec.ErrNotFound, want: DiagToolMissing, fix: "install Xcode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := Spec{AppiumHome: appiumHome(t), Team: "YJ77YV2PNA", BundleID: "app.fixit.WebDriverAgentRunner", CacheDir: t.TempDir()}
			run := &fakeRun{fn: func(c hostexec.Cmd) (hostexec.Result, error) {
				if c.Argv[1] == "-version" {
					return hostexec.Result{Stdout: readFixture(t, "xcodebuild-version.txt")}, nil
				}
				return tc.build, tc.buildErr
			}}
			env := newEnv(t, run)
			env.PipeCapacity = func() (int, error) { return tc.pipe, nil }
			_, err := Build(context.Background(), env, spec, BuildOptions{})
			var d runx.DiagError
			if !errors.As(err, &d) || d.Diag.Code != tc.want || !strings.Contains(d.Diag.Fix, tc.fix) {
				t.Fatalf("got %v, want %s with fix containing %q", err, tc.want, tc.fix)
			}
			if tc.want == DiagPipeCapacityLow && run.ran("xcodebuild build-for-testing") != 0 {
				t.Fatal("xcodebuild ran despite the pipe refusal")
			}
			leftovers, _ := filepath.Glob(filepath.Join(spec.CacheDir, "wda", "wda1-*"))
			if len(leftovers) != 0 {
				t.Fatalf("a failed build was indexed: %v", leftovers)
			}
		})
	}
}

func TestSpecAndToolchainUsage(t *testing.T) {
	run := &fakeRun{fn: func(hostexec.Cmd) (hostexec.Result, error) {
		return hostexec.Result{Stdout: readFixture(t, "xcodebuild-version.txt")}, nil
	}}
	env := newEnv(t, run)
	if _, err := Find(context.Background(), env, Spec{AppiumHome: "/x", BundleID: "b", CacheDir: "/c"}); diagCode(t, err) != DiagUsage {
		t.Fatal("no team must be USAGE")
	}
	empty := t.TempDir()
	_, err := Find(context.Background(), env, Spec{AppiumHome: empty, Team: "T", BundleID: "b", CacheDir: t.TempDir()})
	var d runx.DiagError
	if !errors.As(err, &d) || d.Diag.Code != DiagAppiumDriverMissing || d.Diag.Fix != "APPIUM_HOME="+empty+" appium driver install xcuitest" {
		t.Fatalf("missing driver: %v", err)
	}
}

func TestImport(t *testing.T) {
	cases := []struct {
		name       string
		xcodeBuild string // DTXcodeBuild of the source
		runnerID   string
		want       string // diag code, "" = imported
	}{
		// The calendar campaign's /tmp/calendar-perf/wda-dd was built by
		// 17F41; this Mac updated to 17F42 on 2026-10-02.
		{"built by an older Xcode is not reused", "17F41", "app.fixit.WebDriverAgentRunner.xctrunner", DiagWDAMissing},
		{"another bundle id names the right flag", "17F42", "com.example.WebDriverAgentRunner.xctrunner", DiagUsage},
		{"same toolchain imports", "17F42", "app.fixit.WebDriverAgentRunner.xctrunner", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := t.TempDir()
			writeProducts(t, src)
			spec := Spec{AppiumHome: appiumHome(t), Team: "YJ77YV2PNA", BundleID: "app.fixit.WebDriverAgentRunner", CacheDir: t.TempDir()}
			run := &fakeRun{fn: func(c hostexec.Cmd) (hostexec.Result, error) {
				switch c.Argv[0] {
				case "xcodebuild":
					return hostexec.Result{Stdout: readFixture(t, "xcodebuild-version.txt")}, nil
				case "plutil":
					if c.Argv[2] == "DTXcodeBuild" {
						return hostexec.Result{Stdout: []byte(tc.xcodeBuild + "\n")}, nil
					}
					return hostexec.Result{Stdout: []byte(tc.runnerID + "\n")}, nil
				case "codesign":
					return hostexec.Result{Stderr: readFixture(t, "codesign-dv.txt")}, nil
				case "ditto":
					writeProducts(t, c.Argv[2])
					return hostexec.Result{}, nil
				}
				t.Fatalf("unexpected %v", c.Argv)
				return hostexec.Result{}, nil
			}}
			res, err := Import(context.Background(), newEnv(t, run), spec, src)
			if tc.want != "" {
				if code := diagCode(t, err); code != tc.want {
					t.Fatalf("got %s want %s", code, tc.want)
				}
				if run.ran("ditto") != 0 {
					t.Fatal("a refused import copied the tree")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			e := res.Data.(Data).Entry
			if e.ImportedFrom != src || e.Team != "YJ77YV2PNA" {
				t.Fatalf("entry %+v", e)
			}
			if _, err := Find(context.Background(), newEnv(t, run), spec); err != nil {
				t.Fatalf("find after import: %v", err)
			}
		})
	}
}

func mustRead(path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	return b
}

func indexOf(xs []string, x string) int {
	for i, v := range xs {
		if v == x {
			return i
		}
	}
	return -1
}

func indexOfPrefix(xs []string, p string) int {
	for i, v := range xs {
		if strings.HasPrefix(v, p) {
			return i
		}
	}
	return -1
}

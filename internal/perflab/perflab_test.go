package perflab

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/henderson-tech/vybava/internal/runx"
)

// fixitSection is the FixIt adapter section from design 6.1, as JSON.
const fixitSection = `{
  "app": {"root": "apps/client", "entry": "index.js",
    "ios": {"scheme": "FixIt", "bundleId": "app.fixit.client", "team": "XXXXXXXXXX"},
    "android": {"package": "app.fixit.client.dev", "activity": ".MainActivity"}},
  "profiles": {"perf": {"env": "bun scripts/perf/perf-env.ts --platform {platform} --api {deviceApiOrigin}"}},
  "fingerprint": {"cmd": "bunx @expo/fingerprint fingerprint:generate {appRoot} --platform {platform} --debug", "extraNativeInputs": []},
  "build": {"iosBundled": "FIXIT_PERF_DEVICE_KIND=physical FIXIT_PERF_API_URL={deviceApiOrigin} bun run e2e:build:perf -- --install --no-device-install --out {outDir}"},
  "scenarios": "bun scripts/perf/scenarios.ts --json",
  "runner": {"cmd": "bun scripts/perf/run.ts {platform} {scenarios}",
    "env": {"FIXIT_APPIUM_PLATFORM": "{platform}", "FIXIT_APPIUM_IOS_UDID": "{udid}", "FIXIT_APPIUM_ANDROID_UDID": "{serial}",
      "FIXIT_APPIUM_WDA_DERIVED_DATA": "{wdaDerivedData}", "FIXIT_APPIUM_MOCHA_TIMEOUT_MS": "{timeoutMs}", "FIXIT_PERF_OUT_DIR": "{runDir}",
      "FIXIT_PERF_MARKS_FILE": "{runDir}/marks.jsonl"},
    "unset": ["FIXIT_APPIUM_XCODE_ORG_ID", "FIXIT_APPIUM_XCODE_SIGNING_ID"],
    "appiumHome": "appium/.appium-home", "appiumServerLog": "appium/reports/appium-server.log"},
  "api": {"ws": "fixit-work-{branchSlug}", "origin": "devbox url {ws} api", "hold": "devbox hold {ws} --for 4h", "health": "/api/v1/health/ready",
    "device": {"android": {"strategy": "reverse", "devicePort": 23936}, "ios": {"strategy": "bake", "origin": "devbox url {ws} api"}}},
  "hooks": {"signInLink": "bun scripts/perf/login-link.ts --email {account} --redirect {route} --api {deviceApiOrigin}",
    "resetWorld": "bun scripts/perf/reset-world.ts --scenario {world}"},
  "out": "~/Exports/FixIt/perf/{date}-{topic}",
  "hazards": {"ambientGates": ["useAmbientMotion"], "visibilityHint": ["shown", "visible", "isFocused"]}
}`

func decodeSection(t *testing.T, raw string) Config {
	t.Helper()
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestValidate(t *testing.T) {
	if problems := decodeSection(t, fixitSection).Validate(); len(problems) > 0 {
		t.Fatalf("the FixIt section must validate: %v", problems)
	}
	cases := []struct {
		name, from, to, want string
	}{
		{"unknown token", `{branchSlug}`, `{branchSlugg}`, "{branchSlugg} is not an engine token"},
		{"token outside its field", `"devbox url {ws} api", "hold"`, `"devbox url {udid} api", "hold"`, "api.origin: {udid} has no value there"},
		{"bundled build without outDir", ` --out {outDir}`, ``, "build.iosBundled must write its .app into {outDir}"},
		{"bad env name", `"FIXIT_PERF_OUT_DIR"`, `"FIXIT-PERF"`, `runner.env: "FIXIT-PERF" is not an environment variable name`},
		{"reverse strategy only", `"strategy": "reverse"`, `"strategy": "forward"`, `the only strategy is "reverse"`},
		{"extra input shape", `"extraNativeInputs": []`, `"extraNativeInputs": ["package.json"]`, "is not <file>#<json pointer>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := strings.Replace(fixitSection, c.from, c.to, 1)
			if raw == fixitSection {
				t.Fatalf("fixture edit %q did not apply", c.from)
			}
			problems := strings.Join(decodeSection(t, raw).Validate(), "; ")
			if !strings.Contains(problems, c.want) {
				t.Fatalf("problems = %q, want %q", problems, c.want)
			}
		})
	}
	dec := json.NewDecoder(strings.NewReader(strings.Replace(fixitSection, `"scenarios":`, `"scenarioz": "x", "scenarios":`, 1)))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err == nil {
		t.Fatal("an unknown key must be refused")
	}
}

func TestExpand(t *testing.T) {
	v := NewVars(map[string]string{"platform": "android", "runDir": "/x/My Runs/01-before-1"})
	v.SetList("scenarios", []string{"calendar-view-switch-smooth", "calendar mode"})
	calls := 0
	v.Lazy("apiOrigin", func() (string, error) { calls++; return "http://10.8.0.10:21936", nil })
	got, err := v.Expand("runner.cmd", "bun scripts/perf/run.ts {platform} {scenarios} --out {runDir}", true)
	if err != nil {
		t.Fatal(err)
	}
	if want := `bun scripts/perf/run.ts android calendar-view-switch-smooth 'calendar mode' --out '/x/My Runs/01-before-1'`; got != want {
		t.Fatalf("command = %s\nwant      %s", got, want)
	}
	if got, _ := v.Expand("runner.env.X", "{scenarios}|{runDir}/marks.jsonl", false); got != "calendar-view-switch-smooth,calendar mode|/x/My Runs/01-before-1/marks.jsonl" {
		t.Fatalf("env value = %s", got)
	}
	if calls != 0 {
		t.Fatal("a lazy token must not resolve until a template uses it")
	}
	for i := 0; i < 2; i++ {
		if _, err := v.Expand("api.x", "{apiOrigin}${HOME}", false); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatalf("a lazy token resolves once, ran %d times", calls)
	}
	if _, err := v.Expand("runner.cmd", "{udid}", true); CodeOf(err) != DiagConfigInvalid {
		t.Fatalf("an unset token is CONFIG_INVALID, got %v", err)
	}
	if slug("work/marketplace-ui-vt-4229") != "work-marketplace-ui-vt-4229" {
		t.Fatal("branch slug")
	}
}

// TestDiagnosticsDocumented holds the closed enum, the packages' diag
// sources and docs/perflab.md together.
func TestDiagnosticsDocumented(t *testing.T) {
	raw, err := os.ReadFile("../../docs/perflab.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	known := map[string]bool{}
	for _, c := range Codes {
		known[c] = true
		if !strings.Contains(doc, "`"+c+"`") {
			t.Errorf("docs/perflab.md has no row for %s", c)
		}
	}
	codeRe := regexp.MustCompile(`Diag[A-Za-z]+\s*=\s*"([A-Z][A-Z_]+)"`)
	for _, glob := range []string{"*.go", "*/diag.go", "../devlab/diag.go", "../xctrace/diag.go", "../framestats/*.go"} {
		files, _ := filepath.Glob(glob)
		for _, f := range files {
			if strings.HasSuffix(f, "_test.go") {
				continue
			}
			src, _ := os.ReadFile(f)
			for _, m := range codeRe.FindAllStringSubmatch(string(src), -1) {
				if !known[m[1]] {
					t.Errorf("%s declares %s, which perflab.Codes does not list", f, m[1])
				}
			}
		}
	}
}

// The skill's protocol runs `build find --platform <p>` without --kind: on
// android that must name the one kind there is, never a USAGE refusal.
func TestBuildTargetKindDefault(t *testing.T) {
	for _, tc := range []struct{ platform, kind, want string }{
		{"ios", "", "shell"},
		{"android", "", "bundled"},
		{"ios", "bundled", "bundled"},
		{"android", "shell", "shell"}, // explicit misuse stays visible to validate
	} {
		if got := (BuildOptions{Platform: tc.platform, Profile: "perf", Kind: tc.kind}).target().Kind; got != tc.want {
			t.Errorf("%s kind %q: got %q, want %q", tc.platform, tc.kind, got, tc.want)
		}
	}
}

// adb shell joins its argv into one device shell line: the sign-in link's
// `&` must reach `am start` inside one quoted word (the 2026-10-02 S20 link
// died with "app.fixit.client.dev: inaccessible or not found", exit 127).
func TestAndroidLaunchArgsQuoteTheLink(t *testing.T) {
	link := "fixitapp-dev://e2e/login?email=partner1%40e2e-fixit.test&force=1&apiUrl=http%3A%2F%2Flocalhost%3A23936"
	for _, tc := range []struct {
		payload, activity string
		want              string
	}{
		{link, ".MainActivity", "shell am start -a android.intent.action.VIEW -d '" + link + "' app.fixit.client.dev"},
		{"fixitapp-dev://home", "", "shell am start -a android.intent.action.VIEW -d fixitapp-dev://home app.fixit.client.dev"},
		{"", ".MainActivity", "shell am start -n app.fixit.client.dev/.MainActivity"},
		{"", "", "shell monkey -p app.fixit.client.dev -c android.intent.category.LAUNCHER 1"},
	} {
		if got := strings.Join(androidLaunchArgs(tc.payload, tc.activity, "app.fixit.client.dev"), " "); got != tc.want {
			t.Errorf("got  %s\nwant %s", got, tc.want)
		}
	}
}

// A fix a package below the verb layer wrote with the <token> placeholder
// reaches `next` with the holder's token in it.
func TestWithTokenFillsTheFix(t *testing.T) {
	const token = "plt_abc"
	placeholder := runx.DiagError{Diag: runx.Diagnostic{Code: DiagUsage, Fix: "perflab install v1 --device s20 --lease <token> --json"}}
	if de, ok := withToken(placeholder, token).(runx.DiagError); !ok || de.Diag.Fix != "perflab install v1 --device s20 --lease plt_abc --json" {
		t.Fatalf("filled fix: %+v", de)
	}
	plain := errors.New("disk full")
	if withToken(plain, token) != plain || withToken(nil, token) != nil || withToken(placeholder, "") != error(placeholder) {
		t.Fatal("a plain error, nil and an unknown token pass through unchanged")
	}
	rows := diagsWithToken([]runx.Diagnostic{{Fix: "perflab device shell s20 --lease <token> --json -- shell cmd package compile -m speed -f app.fixit.client.dev"}}, token)
	if !strings.Contains(rows[0].Fix, "--lease plt_abc ") {
		t.Fatalf("rows: %+v", rows)
	}
}

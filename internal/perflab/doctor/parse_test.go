package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const (
	iphone11UDID = "00008030-001E6D961122802E"
	airUDID      = "00008150-00116D4C1E40401C"
)

func TestParseXctraceDevices(t *testing.T) {
	// Captured 2026-10-02 with the iPhone 11's screen asleep.
	on, off := parseXctraceDevices(fixture(t, "xctrace-list-iphone11-offline.txt"))
	if !off[iphone11UDID] || on[iphone11UDID] {
		t.Fatalf("iPhone 11 must read offline: on=%v off=%v", on, off)
	}
	if !on["ABF48ABE-4E14-53C7-9463-872006919B3D"] || len(on) != 1 {
		t.Fatalf("only the Mac is online: %v", on)
	}
	if !off[airUDID] || len(off) != 9 {
		t.Fatalf("offline rows: %v", off)
	}
}

func TestParseTunnelRegistry(t *testing.T) {
	reg, err := parseTunnelRegistry([]byte(fixture(t, "tunnels-air-iphone11.json")))
	if err != nil || reg.Status != "OK" || len(reg.Tunnels) != 2 {
		t.Fatalf("%+v %v", reg, err)
	}
	if e := reg.Tunnels[iphone11UDID]; e.Address != "fdcc:a1a5:582b::1" || e.RSDPort != 55936 || e.ConnectionType != "USB" {
		t.Fatalf("iPhone 11 entry %+v", e)
	}
	empty, err := parseTunnelRegistry([]byte(fixture(t, "tunnels-empty.json")))
	if err != nil || empty.Status != "OK" || len(empty.Tunnels) != 0 {
		t.Fatalf("%+v %v", empty, err)
	}
}

func TestParseLoadAndJava(t *testing.T) {
	ncpu, load, err := parseLoad(fixture(t, "sysctl-load.txt"))
	if err != nil || ncpu != 14 || load != 12.02 {
		t.Fatalf("parseLoad = %d %v %v", ncpu, load, err)
	}
	cases := []struct {
		out   string
		major int
		graal bool
	}{
		{fixture(t, "java-version-graalvm21.txt"), 21, true},
		{"openjdk version \"21.0.10\" 2026-01-20\nOpenJDK Runtime Environment Homebrew (build 21.0.10)\n", 21, false},
		{"openjdk version \"17.0.2\" 2022-01-18\n", 17, false},
		{"java version \"1.8.0_292\"\n", 8, false},
	}
	for _, tc := range cases {
		if major, graal := parseJavaVersion(tc.out); major != tc.major || graal != tc.graal {
			t.Errorf("parseJavaVersion(%q) = %d %v, want %d %v", strings.SplitN(tc.out, "\n", 2)[0], major, graal, tc.major, tc.graal)
		}
	}
}

func TestParsePackageDumpAndManifest(t *testing.T) {
	cases := []struct {
		file, pkg  string
		installed  bool
		version    string
		debuggable bool
	}{
		{"dumpsys-package-perf-release.txt", "app.fixit.client", true, "2.6.0", false},
		{"dumpsys-package-debuggable.txt", "io.appium.settings", true, "8.0.9", true},
		{"dumpsys-package-missing.txt", "app.fixit.client.no.such.pkg", false, "", false},
	}
	for _, tc := range cases {
		info := parsePackageDump(fixture(t, tc.file), tc.pkg)
		dbg := false
		for _, f := range info.Flags {
			dbg = dbg || f == "DEBUGGABLE"
		}
		if info.Installed != tc.installed || info.VersionName != tc.version || dbg != tc.debuggable {
			t.Errorf("%s: %+v", tc.file, info)
		}
	}
	// The S20's perf APK (app.fixit.client 2.6.0) vs its dev package (4.0.0).
	if prof, dbg := parseManifest(fixture(t, "aapt2-manifest-profileable.txt")); !prof || dbg {
		t.Errorf("perf APK: profileable=%v debuggable=%v", prof, dbg)
	}
	if prof, _ := parseManifest(fixture(t, "aapt2-manifest-not-profileable.txt")); prof {
		t.Error("dev package read as profileable")
	}
}

func TestParseFocus(t *testing.T) {
	pkg, activity := parseFocus(fixture(t, "dumpsys-window-resolver-focus.txt"))
	if pkg != "android" || activity != "com.android.internal.app.ResolverActivity" {
		t.Fatalf("got %q %q", pkg, activity)
	}
	if pkg, _ := parseFocus("  mCurrentFocus=Window{8f1c2a0 u0 app.fixit.client/app.fixit.client.MainActivity}\n"); pkg != "app.fixit.client" {
		t.Fatalf("got %q", pkg)
	}
	if pkg, _ := parseFocus("  mCurrentFocus=Window{4a1 u0 NotificationShade}\n"); pkg != "NotificationShade" {
		t.Fatalf("got %q", pkg)
	}
	if pkg, _ := parseFocus("  mCurrentFocus=null\n"); pkg != "" {
		t.Fatalf("got %q", pkg)
	}
}

func TestScanAppiumLog(t *testing.T) {
	timeout := fixture(t, "appium-ui-automation-timeout.log")
	ok := fixture(t, "appium-session-ok.log")
	cases := []struct {
		name string
		log  string
		want appiumOutcome
		at   string
	}{
		{"UI Automation off (2026-10-01 before/turn4)", timeout, appiumAutomationTimeout, "2026-10-01 18:34:05"},
		{"session up (after/turn11)", ok, appiumSessionOK, "2026-10-01 20:38:58"},
		{"a later server start that succeeded wins", timeout + ok, appiumSessionOK, "2026-10-01 20:38:58"},
		{"a later server start that timed out wins", ok + timeout, appiumAutomationTimeout, "2026-10-01 18:34:05"},
		{"no WDA attempt yet", "2026-10-01 18:28:38:808 [Appium] Welcome to Appium v3.4.2\n", appiumNone, ""},
	}
	for _, tc := range cases {
		if got, at := scanAppiumLog(tc.log); got != tc.want || at != tc.at {
			t.Errorf("%s: got %s %q, want %s %q", tc.name, got, at, tc.want, tc.at)
		}
	}
}

func TestParseOriginOutput(t *testing.T) {
	const ws = "fixit-work-marketplace-ui-vt-4229"
	text := fixture(t, "devbox-url-parked.txt")
	// devbox prints its fix after an em dash; the fixture stores a hyphen
	// (no long dashes in this repo), so rebuild the real line here.
	realText := strings.Replace(text, " - devbox up", " \u2014 devbox up", 1)
	for name, out := range map[string]string{"text": text, "text as devbox prints it": realText, "json": fixture(t, "devbox-url-parked.json")} {
		origin, parked, fix := parseOriginOutput(out)
		if origin != "" || !strings.Contains(parked, "WS_NOT_RUNNING") || fix != "devbox up "+ws+" --box a" {
			t.Errorf("%s: origin=%q parked=%q fix=%q", name, origin, parked, fix)
		}
	}
	if origin, parked, _ := parseOriginOutput("http://10.8.0.10:21936\n"); origin != "http://10.8.0.10:21936" || parked != "" {
		t.Fatalf("got %q %q", origin, parked)
	}
	if origin, _, _ := parseOriginOutput("no url here"); origin != "" {
		t.Fatalf("got %q", origin)
	}
}

func TestVersionLess(t *testing.T) {
	if !versionLess("35.0.0", "36.0.0") || !versionLess("36.0.0", "36.1.0") || versionLess("36.0.0", "9.0.0") {
		t.Fatal("versionLess")
	}
}

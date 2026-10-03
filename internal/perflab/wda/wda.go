package wda

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/perflab/hostexec"
	"github.com/henderson-tech/vybava/internal/runx"
)

// Spec names the WDA a runner needs. AppiumHome is the adapter's
// `runner.appiumHome` made absolute; CacheDir is the perflab cache root.
type Spec struct {
	AppiumHome string
	Team       string // DEVELOPMENT_TEAM
	BundleID   string // PRODUCT_BUNDLE_IDENTIFIER; the runner app is <id>.xctrunner
	CacheDir   string
}

// Env is how the package reaches the Mac; tests swap every field.
type Env struct {
	Run          hostexec.Runner
	Now          func() time.Time
	Log          io.Writer // progress lines (stderr), never stdout
	PipeCapacity func() (int, error)
	// HostLock takes the Mac-wide build lock for the whole xcodebuild
	// (buildindex owns it; the verb layer wires it). Nil builds unserialised.
	HostLock func(ctx context.Context, key string) (release func(), err error)
}

// DefaultEnv is the real Mac.
func DefaultEnv(log io.Writer) Env {
	return Env{Run: hostexec.OS{}, Now: time.Now, Log: log, PipeCapacity: hostexec.PipeCapacity}
}

// Toolchain is what the key hashes besides the team and bundle id.
type Toolchain struct {
	XcodeVersion  string `json:"xcodeVersion"`
	XcodeBuild    string `json:"xcodeBuild"`
	DriverVersion string `json:"driverVersion"`
	WDAVersion    string `json:"wdaVersion"`
	Project       string `json:"project"`
}

// Entry is one indexed WDA (the manifest beside the derived data).
type Entry struct {
	Key             string    `json:"key"`
	DerivedDataPath string    `json:"derivedDataPath"`
	XCTestRun       string    `json:"xctestrun"`
	BundleID        string    `json:"wdaBundleId"`
	RunnerBundleID  string    `json:"runnerBundleId,omitempty"`
	Team            string    `json:"team"`
	Toolchain       Toolchain `json:"toolchain"`
	BuiltAt         time.Time `json:"builtAt"`
	DurationMs      int64     `json:"durationMs,omitempty"`
	BuildLog        string    `json:"buildLog,omitempty"`
	ImportedFrom    string    `json:"importedFrom,omitempty"`
}

// Data is the find/build/import payload. Tokens are the adapter's
// `{wdaDerivedData}` and `{wdaBundleId}` values for the runner env map.
type Data struct {
	Hit    bool              `json:"hit"`
	Entry  Entry             `json:"entry"`
	Tokens map[string]string `json:"tokens"`
}

// Result is what a verb hands the envelope.
type Result struct {
	Data        any
	Diagnostics []runx.Diagnostic
	Next        []string
}

// KeyVersion prefixes every key; bump it when the hashed inputs change.
const KeyVersion = "wda1"

const (
	manifestName = "perflab-wda.json"
	runnerApp    = "WebDriverAgentRunner-Runner.app"
	// DefaultStall and DefaultTimeout are the design's wda build limits.
	DefaultStall   = 10 * time.Minute
	DefaultTimeout = 20 * time.Minute
)

// DefaultBundleID derives the runner bundle id from the app's: the app's
// parent domain + ".WebDriverAgentRunner" (app.fixit.client ->
// app.fixit.WebDriverAgentRunner), which the team can provision.
func DefaultBundleID(appBundleID string) string {
	if i := strings.LastIndex(appBundleID, "."); i > 0 {
		return appBundleID[:i] + ".WebDriverAgentRunner"
	}
	if appBundleID == "" {
		return ""
	}
	return appBundleID + ".WebDriverAgentRunner"
}

// BuildCommand is the exact fix line WDA_MISSING names.
func BuildCommand(s Spec) string {
	return fmt.Sprintf("perflab wda build --team %s --bundle-id %s --json", orPlaceholder(s.Team, "<TEAMID>"), orPlaceholder(s.BundleID, "<bundle-id>"))
}

func orPlaceholder(v, p string) string {
	if v == "" {
		return p
	}
	return v
}

func (s Spec) validate() error {
	switch {
	case s.AppiumHome == "":
		return diag(DiagUsage, "no Appium home: the adapter's runner.appiumHome is empty", "perflab adapter check --json")
	case s.Team == "":
		return diag(DiagUsage, "no signing team for WebDriverAgent (list the Mac's teams with `security find-identity -v -p codesigning`)", BuildCommand(s))
	case s.BundleID == "":
		return diag(DiagUsage, "no WebDriverAgent bundle id", BuildCommand(s))
	case s.CacheDir == "":
		return diag(DiagUsage, "no perflab cache dir", "set PERFLAB_CACHE_DIR or re-run without it")
	}
	return nil
}

func (s Spec) wdaDir() string { return filepath.Join(s.CacheDir, "wda") }

// Key hashes the toolchain, team and bundle id: `wda1-<16 hex>`.
func Key(tc Toolchain, s Spec) string {
	lines := []string{
		KeyVersion,
		"xcodeBuild\t" + tc.XcodeBuild,
		"driver\t" + tc.DriverVersion,
		"wda\t" + tc.WDAVersion,
		"team\t" + s.Team,
		"bundleId\t" + s.BundleID,
	}
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return KeyVersion + "-" + hex.EncodeToString(sum[:])[:16]
}

var (
	xcodeVersionLine = regexp.MustCompile(`(?m)^Xcode\s+(\S+)`)
	xcodeBuildLine   = regexp.MustCompile(`(?m)^Build version\s+(\S+)`)
)

// parseXcodeVersion reads `xcodebuild -version`.
func parseXcodeVersion(out string) (version, build string) {
	if m := xcodeVersionLine.FindStringSubmatch(out); m != nil {
		version = m[1]
	}
	if m := xcodeBuildLine.FindStringSubmatch(out); m != nil {
		build = m[1]
	}
	return version, build
}

// ResolveToolchain reads Xcode's build and the driver + WebDriverAgent
// versions under the Appium home.
func ResolveToolchain(ctx context.Context, env Env, appiumHome string) (Toolchain, error) {
	var tc Toolchain
	res, err := env.Run.Run(ctx, hostexec.Cmd{Argv: []string{"xcodebuild", "-version"}, Timeout: 30 * time.Second})
	if err != nil {
		if hostexec.NotFound(err) {
			return tc, diag(DiagToolMissing, "xcodebuild is not installed", "install Xcode, then sudo xcode-select -s /Applications/Xcode.app")
		}
		return tc, err
	}
	tc.XcodeVersion, tc.XcodeBuild = parseXcodeVersion(string(res.Stdout))
	if res.Exit != 0 || tc.XcodeBuild == "" {
		return tc, diag(DiagToolMissing, "xcodebuild -version failed: "+res.Tail(), "sudo xcode-select -s /Applications/Xcode.app && xcodebuild -runFirstLaunch")
	}
	install := fmt.Sprintf("APPIUM_HOME=%s appium driver install xcuitest", appiumHome)
	driverDir := filepath.Join(appiumHome, "node_modules", "appium-xcuitest-driver")
	tc.DriverVersion, err = packageVersion(driverDir)
	if err != nil {
		return tc, diag(DiagAppiumDriverMissing, "no xcuitest driver under "+appiumHome+": "+err.Error(), install)
	}
	for _, dir := range []string{
		filepath.Join(driverDir, "node_modules", "appium-webdriveragent"),
		filepath.Join(appiumHome, "node_modules", "appium-webdriveragent"),
	} {
		proj := filepath.Join(dir, "WebDriverAgent.xcodeproj")
		if _, statErr := os.Stat(proj); statErr != nil {
			continue
		}
		if tc.WDAVersion, err = packageVersion(dir); err == nil {
			tc.Project = proj
			return tc, nil
		}
	}
	return tc, diag(DiagAppiumDriverMissing, "the xcuitest driver under "+appiumHome+" ships no appium-webdriveragent project", install)
}

func packageVersion(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return "", err
	}
	var pkg struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(b, &pkg); err != nil {
		return "", fmt.Errorf("%s/package.json: %w", dir, err)
	}
	if pkg.Version == "" {
		return "", fmt.Errorf("%s/package.json has no version", dir)
	}
	return pkg.Version, nil
}

func tokens(e Entry) map[string]string {
	return map[string]string{"wdaDerivedData": e.DerivedDataPath, "wdaBundleId": e.BundleID}
}

func doneNext() []string { return []string{"perflab doctor --platform ios --for run --json"} }

// load returns the indexed entry for key when its manifest and xctestrun
// are both present (a deleted product reads as missing).
func load(s Spec, key string) (Entry, bool) {
	dir := filepath.Join(s.wdaDir(), key)
	b, err := os.ReadFile(filepath.Join(dir, manifestName))
	if err != nil {
		return Entry{}, false
	}
	var e Entry
	if json.Unmarshal(b, &e) != nil {
		return Entry{}, false
	}
	e.DerivedDataPath = dir
	run, ok := findXCTestRun(dir)
	if !ok {
		return Entry{}, false
	}
	e.XCTestRun = run
	return e, true
}

// findXCTestRun returns the build's generic xctestrun
// (WebDriverAgentRunner_iphoneos<sdk>-arm64.xctestrun). Appium's per-device
// copies (<udid>_<sdk>.xctestrun) never match.
func findXCTestRun(derived string) (string, bool) {
	matches, _ := filepath.Glob(filepath.Join(derived, "Build", "Products", "WebDriverAgentRunner_iphoneos*.xctestrun"))
	if len(matches) == 0 {
		return "", false
	}
	sort.Strings(matches)
	return matches[len(matches)-1], true
}

// Find answers the indexed WDA for this Mac's toolchain, or WDA_MISSING.
func Find(ctx context.Context, env Env, s Spec) (Result, error) {
	if err := s.validate(); err != nil {
		return Result{}, err
	}
	tc, err := ResolveToolchain(ctx, env, s.AppiumHome)
	if err != nil {
		return Result{}, err
	}
	key := Key(tc, s)
	e, ok := load(s, key)
	if !ok {
		return Result{}, missing(tc, s, key)
	}
	return Result{Data: Data{Hit: true, Entry: e, Tokens: tokens(e)}, Next: doneNext()}, nil
}

func missing(tc Toolchain, s Spec, key string) error {
	return diag(DiagWDAMissing, fmt.Sprintf("no prebuilt WebDriverAgent %s for Xcode %s (%s), xcuitest %s, WDA %s, team %s, bundle id %s",
		key, tc.XcodeVersion, tc.XcodeBuild, tc.DriverVersion, tc.WDAVersion, s.Team, s.BundleID), BuildCommand(s))
}

// BuildOptions are the build's watchdogs.
type BuildOptions struct {
	Stall   time.Duration
	Timeout time.Duration
}

// buildArgv is the proven recipe: no device contact, signed for the team.
func buildArgv(tc Toolchain, s Spec, derived string) []string {
	return []string{
		"xcodebuild", "build-for-testing",
		"-project", tc.Project,
		"-scheme", "WebDriverAgentRunner",
		"-destination", "generic/platform=iOS",
		"-derivedDataPath", derived,
		"-allowProvisioningUpdates",
		"DEVELOPMENT_TEAM=" + s.Team,
		"PRODUCT_BUNDLE_IDENTIFIER=" + s.BundleID,
	}
}

var signingLine = regexp.MustCompile(`(?m)^.*(No Account for Team|requires a development team|No profiles for|Signing for "[^"]+" requires|No signing certificate).*$`)

// platformMissing is Xcode 26 refusing every iOS destination because the iOS
// platform matching its SDK was never downloaded (an Xcode update leaves it
// out): "iOS 26.5 is not installed. Please download and install the platform".
var platformMissing = regexp.MustCompile(`iOS (\S+) is not installed\. Please download and install the platform`)

// Build returns the indexed WDA, building it on a miss. A hit never runs
// xcodebuild; a miss runs the pipe probe first, takes the host build lock,
// builds into a temp dir and renames it into place.
func Build(ctx context.Context, env Env, s Spec, opts BuildOptions) (Result, error) {
	if err := s.validate(); err != nil {
		return Result{}, err
	}
	if opts.Stall <= 0 {
		opts.Stall = DefaultStall
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	tc, err := ResolveToolchain(ctx, env, s.AppiumHome)
	if err != nil {
		return Result{}, err
	}
	key := Key(tc, s)
	if e, ok := load(s, key); ok {
		return Result{Data: Data{Hit: true, Entry: e, Tokens: tokens(e)}, Next: doneNext()}, nil
	}
	prog := hostexec.NewProgress(env.Log, "wda build", env.Now)
	prog.Phase("pipe-probe")
	if env.PipeCapacity != nil {
		if n, perr := env.PipeCapacity(); perr == nil && n < hostexec.PipeCapacityLow {
			return Result{}, diag(DiagPipeCapacityLow, fmt.Sprintf("a fresh kernel pipe buffers %d B (< %d): xcodebuild would hang at \"Planning build\"", n, hostexec.PipeCapacityLow),
				"close idle Claude/Codex sessions, then perflab doctor --for build --json")
		}
	}
	if env.HostLock != nil {
		prog.Phase("host-lock")
		release, lerr := env.HostLock(ctx, key)
		if lerr != nil {
			return Result{}, lerr
		}
		defer release()
		if e, ok := load(s, key); ok {
			return Result{Data: Data{Hit: true, Entry: e, Tokens: tokens(e)}, Next: doneNext()}, nil
		}
	}
	if err := os.MkdirAll(filepath.Join(s.wdaDir(), "logs"), 0o755); err != nil {
		return Result{}, err
	}
	tmp, err := os.MkdirTemp(s.wdaDir(), ".tmp-"+key+"-")
	if err != nil {
		return Result{}, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(tmp)
		}
	}()
	start := env.Now()
	logPath := filepath.Join(s.wdaDir(), "logs", fmt.Sprintf("%s-%d.log", key, start.Unix()))
	logFile, err := os.Create(logPath)
	if err != nil {
		return Result{}, err
	}
	defer logFile.Close()

	prog.Phase("xcodebuild", "key="+key)
	hbCtx, stopHB := context.WithCancel(ctx)
	go prog.Heartbeat(hbCtx, 30*time.Second, nil)
	res, err := env.Run.Run(ctx, hostexec.Cmd{Argv: buildArgv(tc, s, tmp), Log: logFile, Timeout: opts.Timeout, Stall: opts.Stall})
	stopHB()
	if err != nil {
		if hostexec.NotFound(err) {
			return Result{}, diag(DiagToolMissing, "xcodebuild is not installed", "install Xcode, then sudo xcode-select -s /Applications/Xcode.app")
		}
		return Result{}, err
	}
	switch {
	case res.Stalled:
		return Result{}, diag(DiagBuildStalled, fmt.Sprintf("xcodebuild printed nothing for %s (log %s)", opts.Stall, logPath),
			"perflab doctor --for build --json (pipe probe), then "+BuildCommand(s))
	case res.TimedOut:
		return Result{}, diag(DiagBuildFailed, fmt.Sprintf("xcodebuild did not finish within %s (log %s)", opts.Timeout, logPath), "tail -n 80 "+logPath)
	case res.Exit != 0:
		detail := fmt.Sprintf("xcodebuild build-for-testing exited %d (log %s): %s", res.Exit, logPath, res.Tail())
		fix := "tail -n 80 " + logPath
		if m := platformMissing.FindStringSubmatch(string(res.Stdout)); m != nil {
			return Result{}, diag(DiagToolMissing, fmt.Sprintf("the iOS %s platform is not installed, so xcodebuild has no iOS destination (log %s)", m[1], logPath), "xcodebuild -downloadPlatform iOS, then "+BuildCommand(s))
		}
		if m := signingLine.FindString(string(res.Stdout)); m != "" {
			detail = fmt.Sprintf("WebDriverAgent signing failed for team %s (log %s): %s", s.Team, logPath, strings.TrimSpace(m))
			fix = "sign the team in at Xcode > Settings > Accounts, then " + BuildCommand(s)
		}
		return Result{}, diag(DiagBuildFailed, detail, fix)
	}
	run, ok := findXCTestRun(tmp)
	if !ok {
		return Result{}, diag(DiagBuildFailed, "xcodebuild succeeded but left no WebDriverAgentRunner_iphoneos*.xctestrun (log "+logPath+")", "tail -n 80 "+logPath)
	}
	entry := Entry{
		Key: key, BundleID: s.BundleID, Team: s.Team, Toolchain: tc,
		RunnerBundleID: runnerBundleID(ctx, env, tmp),
		BuiltAt:        start.UTC(), DurationMs: res.Duration.Milliseconds(), BuildLog: logPath,
	}
	final, err := place(s, key, tmp, entry, run)
	if err != nil {
		return Result{}, err
	}
	keep = true
	prog.Phase("done", "key="+key)
	return Result{Data: Data{Hit: false, Entry: final, Tokens: tokens(final)}, Next: doneNext()}, nil
}

// place writes the manifest into tmp and renames tmp into <wda>/<key>. A
// key that appeared meanwhile wins (the build is identical) and tmp is
// dropped.
func place(s Spec, key, tmp string, e Entry, run string) (Entry, error) {
	rel, err := filepath.Rel(tmp, run)
	if err != nil {
		return Entry{}, err
	}
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return Entry{}, err
	}
	if err := os.WriteFile(filepath.Join(tmp, manifestName), append(b, '\n'), 0o644); err != nil {
		return Entry{}, err
	}
	final := filepath.Join(s.wdaDir(), key)
	if err := os.Rename(tmp, final); err != nil {
		if existing, ok := load(s, key); ok {
			_ = os.RemoveAll(tmp)
			return existing, nil
		}
		return Entry{}, err
	}
	e.DerivedDataPath = final
	e.XCTestRun = filepath.Join(final, rel)
	return e, nil
}

func runnerInfoPlist(derived string) string {
	return filepath.Join(derived, "Build", "Products", "Debug-iphoneos", runnerApp, "Info.plist")
}

// plistValue reads one key of a (binary or XML) plist through plutil.
func plistValue(ctx context.Context, env Env, path, key string) (string, error) {
	res, err := env.Run.Run(ctx, hostexec.Cmd{Argv: []string{"plutil", "-extract", key, "raw", "-o", "-", path}, Timeout: 15 * time.Second})
	if err != nil {
		return "", err
	}
	if res.Exit != 0 {
		return "", errors.New(res.Tail())
	}
	return strings.TrimSpace(string(res.Stdout)), nil
}

func runnerBundleID(ctx context.Context, env Env, derived string) string {
	v, _ := plistValue(ctx, env, runnerInfoPlist(derived), "CFBundleIdentifier")
	return v
}

var teamLine = regexp.MustCompile(`(?m)^TeamIdentifier=(\S+)`)

// Import adopts an existing WDA derived data dir (one built by hand, or by
// Appium) when it was built by this Mac's Xcode for the same team and
// bundle id. The driver version cannot be read back from a build, so the
// current one is recorded and the source path is kept as provenance.
func Import(ctx context.Context, env Env, s Spec, dir string) (Result, error) {
	if err := s.validate(); err != nil {
		return Result{}, err
	}
	tc, err := ResolveToolchain(ctx, env, s.AppiumHome)
	if err != nil {
		return Result{}, err
	}
	key := Key(tc, s)
	if e, ok := load(s, key); ok {
		return Result{Data: Data{Hit: true, Entry: e, Tokens: tokens(e)}, Next: doneNext()}, nil
	}
	src, err := filepath.Abs(dir)
	if err != nil {
		return Result{}, err
	}
	if _, ok := findXCTestRun(src); !ok {
		return Result{}, diag(DiagUsage, src+" holds no Build/Products/WebDriverAgentRunner_iphoneos*.xctestrun (not a WDA build-for-testing derived data dir)", BuildCommand(s))
	}
	plist := runnerInfoPlist(src)
	builtWith, err := plistValue(ctx, env, plist, "DTXcodeBuild")
	if err != nil {
		return Result{}, diag(DiagUsage, "cannot read "+plist+": "+err.Error(), BuildCommand(s))
	}
	if builtWith != tc.XcodeBuild {
		return Result{}, diag(DiagWDAMissing, fmt.Sprintf("%s was built by Xcode build %s; this Mac runs Xcode %s (%s), so it is not reused", src, builtWith, tc.XcodeVersion, tc.XcodeBuild), BuildCommand(s))
	}
	runnerID, err := plistValue(ctx, env, plist, "CFBundleIdentifier")
	if err != nil {
		return Result{}, diag(DiagUsage, "cannot read "+plist+": "+err.Error(), BuildCommand(s))
	}
	if got := strings.TrimSuffix(runnerID, ".xctrunner"); got != s.BundleID {
		fixed := s
		fixed.BundleID = got
		return Result{}, diag(DiagUsage, fmt.Sprintf("%s was built for bundle id %s, not %s", src, got, s.BundleID), strings.Replace(BuildCommand(fixed), "wda build", "wda import "+src, 1))
	}
	sig, err := env.Run.Run(ctx, hostexec.Cmd{Argv: []string{"codesign", "-dv", filepath.Dir(plist)}, Timeout: 30 * time.Second})
	if err != nil {
		return Result{}, err
	}
	if m := teamLine.FindStringSubmatch(sig.Combined()); m == nil || m[1] != s.Team {
		got := "an unsigned build"
		if m != nil {
			got = "team " + m[1]
		}
		return Result{}, diag(DiagUsage, fmt.Sprintf("%s is signed by %s, not team %s", src, got, s.Team), BuildCommand(s))
	}
	if err := os.MkdirAll(s.wdaDir(), 0o755); err != nil {
		return Result{}, err
	}
	tmp, err := os.MkdirTemp(s.wdaDir(), ".tmp-"+key+"-")
	if err != nil {
		return Result{}, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(tmp)
		}
	}()
	cp, err := env.Run.Run(ctx, hostexec.Cmd{Argv: []string{"ditto", src, tmp}, Timeout: 10 * time.Minute})
	if err != nil {
		return Result{}, err
	}
	if cp.Exit != 0 {
		return Result{}, fmt.Errorf("ditto %s: %s", src, cp.Tail())
	}
	run, ok := findXCTestRun(tmp)
	if !ok {
		return Result{}, fmt.Errorf("ditto %s left no xctestrun in %s", src, tmp)
	}
	entry := Entry{Key: key, BundleID: s.BundleID, RunnerBundleID: runnerID, Team: s.Team, Toolchain: tc, BuiltAt: env.Now().UTC(), ImportedFrom: src}
	final, err := place(s, key, tmp, entry, run)
	if err != nil {
		return Result{}, err
	}
	keep = true
	return Result{Data: Data{Hit: false, Entry: final, Tokens: tokens(final)}, Next: doneNext()}, nil
}

// List returns every indexed WDA, newest first.
func List(s Spec) ([]Entry, error) {
	if s.CacheDir == "" {
		return nil, diag(DiagUsage, "no perflab cache dir", "set PERFLAB_CACHE_DIR or re-run without it")
	}
	dirs, err := os.ReadDir(s.wdaDir())
	if errors.Is(err, os.ErrNotExist) {
		return []Entry{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Entry{}
	for _, d := range dirs {
		if !d.IsDir() || !strings.HasPrefix(d.Name(), KeyVersion+"-") {
			continue
		}
		if e, ok := load(s, d.Name()); ok {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BuiltAt.After(out[j].BuiltAt) })
	return out, nil
}

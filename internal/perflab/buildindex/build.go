package buildindex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/runx"
)

// BuildSpec is one `perflab build native` invocation.
type BuildSpec struct {
	Project Project
	Target  Target
	Env     ProfileEnv
	// Fingerprint, when set, is the already computed key of Project.
	Fingerprint *Fingerprint
	// Team overrides the adapter's iOS team (--team).
	Team string
	// Ref builds from a detached source worktree at that git ref
	// (--isolated --ref).
	Ref string
	// Wait bounds the waits for the key lock and the host build lock.
	Wait time.Duration
	// Stall is the no-output watchdog (default 10 min), Timeout the hard
	// limit (default 60 min).
	Stall   time.Duration
	Timeout time.Duration
	// Preflight is doctor's host subset (pipe probe first); nil skips it.
	Preflight func(context.Context) error
	Progress  *Progress
}

// BuildResult is the `build native` / `build find` / `build import` payload.
type BuildResult struct {
	Hit         bool              `json:"hit"`
	Build       Build             `json:"build"`
	Fingerprint Fingerprint       `json:"fingerprint"`
	Diagnostics []runx.Diagnostic `json:"-"`
	Next        []string          `json:"-"`
}

const (
	defaultStall        = 10 * time.Minute
	defaultBuildTimeout = 60 * time.Minute
)

// Find is `perflab build find`: the current tree's key looked up.
func (s Store) Find(ctx context.Context, r Runner, spec BuildSpec) (BuildResult, error) {
	fp, err := spec.fingerprint(ctx, r)
	if err != nil {
		return BuildResult{}, err
	}
	b, err := s.FindBuild(spec.Target, fp.Key)
	if err != nil {
		return BuildResult{Fingerprint: fp}, err
	}
	return BuildResult{Hit: true, Build: b, Fingerprint: fp, Next: nextAfterBuild(b)}, nil
}

func (spec *BuildSpec) fingerprint(ctx context.Context, r Runner) (Fingerprint, error) {
	if err := spec.Target.validate(); err != nil {
		return Fingerprint{}, err
	}
	if spec.Team != "" {
		spec.Project.IOS.Team = spec.Team
	}
	if spec.Fingerprint != nil {
		return *spec.Fingerprint, nil
	}
	return RunFingerprint(ctx, r, FingerprintSpec{Project: spec.Project, Target: spec.Target, Env: spec.Env.Vars})
}

// BuildNative is `perflab build native`: an index hit answers at once; a
// miss takes the key lock and the host build lock, runs the recipe under a
// stall watchdog and stores the artifact immutably.
func (s Store) BuildNative(ctx context.Context, r Runner, spec BuildSpec) (BuildResult, error) {
	if err := spec.Target.validate(); err != nil {
		return BuildResult{}, err
	}
	projectApp := spec.Project.appRootAbs()
	if spec.Ref != "" {
		src, err := EnsureSourceWorktree(ctx, r, spec.Project.RepoRoot, spec.Ref, spec.Progress)
		if err != nil {
			return BuildResult{}, err
		}
		spec.Project.RepoRoot = src.Dir
		spec.Fingerprint = nil
	}
	phase(spec.Progress, "fingerprint")
	var fp Fingerprint
	err := withProjectRules(projectApp, spec.Project.appRootAbs(), func() (ferr error) {
		fp, ferr = spec.fingerprint(ctx, r)
		return ferr
	})
	if err != nil {
		return BuildResult{}, err
	}
	if b, err := s.FindBuild(spec.Target, fp.Key); err == nil {
		return BuildResult{Hit: true, Build: b, Fingerprint: fp, Next: nextAfterBuild(b)}, nil
	}

	holder := LockHolder{Verb: "build native", Key: fp.Key, Worktree: filepath.Base(spec.Project.RepoRoot)}
	unlockKey, current, err := s.Dirs.takeExclusive("build-"+fp.Key+".lock", spec.Wait, holder)
	if errors.Is(err, errLockTimeout) {
		who := "another process"
		if current != nil {
			who = current.String()
		}
		return BuildResult{Fingerprint: fp}, diag(DiagBuildInProgress, "key "+fp.Key+" is being built by "+who,
			spec.Target.invocation()+" --wait 60m (the finished build answers as an index hit)")
	}
	if err != nil {
		return BuildResult{}, err
	}
	defer unlockKey()
	if b, err := s.FindBuild(spec.Target, fp.Key); err == nil {
		return BuildResult{Hit: true, Build: b, Fingerprint: fp, Next: nextAfterBuild(b)}, nil
	}
	unlockHost, err := s.Dirs.acquireHostBuild(spec.Wait, holder, spec.Target.invocation())
	if err != nil {
		return BuildResult{Fingerprint: fp}, err
	}
	defer unlockHost()
	if spec.Preflight != nil {
		phase(spec.Progress, "preflight")
		if err := spec.Preflight(ctx); err != nil {
			return BuildResult{Fingerprint: fp}, err
		}
	}

	st, err := newStage(s.buildDir(spec.Target, fp.Key))
	if err != nil {
		return BuildResult{}, err
	}
	logPath := filepath.Join(st.Dir, "build.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		st.discard()
		return BuildResult{}, err
	}
	stopBeat := heartbeat(spec.Progress, 30*time.Second)
	started := time.Now()
	b := &builder{ctx: ctx, r: r, spec: spec, fp: fp, stage: st.Dir, log: logFile, cache: s.Dirs.Cache}
	artifact, prov, err := b.run()
	stopBeat()
	_ = logFile.Close()
	if err != nil {
		kept := s.keepFailedLog(logPath, fp.Key)
		st.discard()
		return BuildResult{Fingerprint: fp}, withLog(err, kept)
	}
	phase(spec.Progress, "index")
	m, err := describeArtifact(ctx, r, spec.Project, spec.Target, fp, artifact, st.Dir)
	if err != nil {
		st.discard()
		return BuildResult{Fingerprint: fp}, err
	}
	m.BuildLog = "build.log"
	m.DurationMs = time.Since(started).Milliseconds()
	m.ProductionEquivalent = prov.ProductionEquivalent
	m.AndroidAssets = prov.AndroidAssets
	m.PublicEnvHash, m.EnvNames = spec.Env.Hash(), spec.Env.Names()
	if prov.PublicEnvHash != "" {
		m.PublicEnvHash = prov.PublicEnvHash
	}
	if m.Source, err = ReadSourceIdentity(ctx, r, spec.Project.RepoRoot, spec.Project.ID); err != nil {
		st.discard()
		return BuildResult{}, err
	}
	return s.commitBuild(st, m, fp)
}

func (s Store) commitBuild(st *stage, m BuildManifest, fp Fingerprint) (BuildResult, error) {
	if err := os.WriteFile(filepath.Join(st.Dir, "key-lines.txt"), []byte(strings.Join(fp.Lines, "\n")+"\n"), 0o644); err != nil {
		st.discard()
		return BuildResult{}, err
	}
	if err := writeJSONAtomic(filepath.Join(st.Dir, manifestFile), m); err != nil {
		st.discard()
		return BuildResult{}, err
	}
	existed, err := st.commit()
	if err != nil {
		return BuildResult{}, err
	}
	b, err := readBuild(st.final)
	if err != nil {
		return BuildResult{}, err
	}
	res := BuildResult{Hit: existed, Build: b, Fingerprint: fp, Next: nextAfterBuild(b)}
	if m.Kind == KindBundled && !m.ProductionEquivalent && m.Platform == "ios" {
		res.Diagnostics = append(res.Diagnostics, info(DiagNotProductionEquivalent,
			"the bundled build's provenance does not mark it production-equivalent", ""))
	}
	return res, nil
}

func nextAfterBuild(b Build) []string {
	m := b.Manifest
	if m.Kind == KindShell || m.Platform == "android" {
		return []string{
			fmt.Sprintf("perflab bundle export --platform %s --profile %s --json", m.Platform, m.Profile),
			fmt.Sprintf("perflab pack --native %s --bundle <sha> --json", m.Key),
		}
	}
	return []string{fmt.Sprintf("perflab install %s --device <id> --lease <token> --json", m.Key)}
}

// keepFailedLog moves a failed build's log out of the discarded stage.
func (s Store) keepFailedLog(logPath, key string) string {
	dir := filepath.Join(s.Dirs.Cache, "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	dst := filepath.Join(dir, fmt.Sprintf("build-%s-%s.log", key, time.Now().UTC().Format("20060102T150405Z")))
	if err := os.Rename(logPath, dst); err != nil {
		return ""
	}
	return dst
}

func withLog(err error, logPath string) error {
	var d runx.DiagError
	if !errors.As(err, &d) {
		return err
	}
	if logPath == "" {
		logPath = "the build log (lost)"
	}
	d.Diag.Detail = strings.ReplaceAll(d.Diag.Detail, "{log}", logPath)
	d.Diag.Fix = strings.ReplaceAll(d.Diag.Fix, "{log}", logPath)
	return d
}

// provenance is what a recipe learns about the artifact beyond the files.
type provenance struct {
	ProductionEquivalent bool
	PublicEnvHash        string
	AndroidAssets        map[string]string
}

type builder struct {
	ctx   context.Context
	r     Runner
	spec  BuildSpec
	fp    Fingerprint
	stage string
	log   *os.File
	cache string
}

func (b *builder) run() (string, provenance, error) {
	t := b.spec.Target
	switch {
	case t.Platform == "ios" && t.Kind == KindShell:
		return b.iosShell()
	case t.Platform == "ios":
		return b.adapterBuild(b.spec.Project.IOSBundledCmd, ".app", "perflab.build.iosBundled")
	case len(b.spec.Project.AndroidBuildCmd) > 0:
		return b.adapterBuild(b.spec.Project.AndroidBuildCmd, ".apk", "perflab.build.android")
	default:
		return b.androidGradle()
	}
}

func (b *builder) env(extra ...string) []string {
	env := append([]string{"EXPO_NO_DOTENV=1", "CI=1", "SENTRY_DISABLE_AUTO_UPLOAD=true"}, b.spec.Env.Vars...)
	return append(env, extra...)
}

func (b *builder) timeout() time.Duration {
	if b.spec.Timeout > 0 {
		return b.spec.Timeout
	}
	return defaultBuildTimeout
}

func (b *builder) stall() time.Duration {
	if b.spec.Stall > 0 {
		return b.spec.Stall
	}
	return defaultStall
}

// step runs one recipe command with the log, the watchdog and the timeout,
// mapping a stall, a timeout or a non-zero exit to its diagnostic.
func (b *builder) step(name string, c Cmd) error {
	phase(b.spec.Progress, name)
	fmt.Fprintf(b.log, "\n$ (cd %s && %s)\n", c.Dir, strings.Join(c.Argv, " "))
	c.Log = b.log
	if c.Stall == 0 {
		c.Stall = b.stall()
	}
	if c.Timeout == 0 {
		c.Timeout = b.timeout()
	}
	res, err := run(b.ctx, b.r, c)
	if err != nil {
		return err
	}
	switch {
	case res.Stalled:
		return diag(DiagBuildStalled,
			fmt.Sprintf("%s printed nothing for %s and was stopped (log {log})", name, c.Stall),
			"perflab doctor --for build --json (the pipe probe catches the 'Planning build' hang), then re-run")
	case res.TimedOut:
		return diag(DiagBuildFailed, fmt.Sprintf("%s hit the %s build timeout (log {log})", name, c.Timeout),
			"re-run with a longer --timeout, after reading {log}")
	case res.Exit != 0:
		return diag(DiagBuildFailed, fmt.Sprintf("%s exited %d: %s (log {log})", name, res.Exit, lastLines(res.Stdout, 3)),
			"read {log}, fix the cause, re-run the same perflab build native")
	}
	return nil
}

// iosShell: prebuild, then a Release xcodebuild without JS bundling.
func (b *builder) iosShell() (string, provenance, error) {
	p := b.spec.Project
	if p.IOS.Scheme == "" || p.IOS.Team == "" {
		return "", provenance{}, diag(DiagUsage, "the adapter names no iOS scheme or team", "set perflab.app.ios.scheme and .team in vybava.config.ts")
	}
	appRoot := p.appRootAbs()
	if err := b.step("prebuild", Cmd{Argv: append(expoCLI(p.RepoRoot), "prebuild", "--platform", "ios"), Dir: appRoot, Env: b.env()}); err != nil {
		return "", provenance{}, err
	}
	derived := filepath.Join(b.cache, "derived", "ios-"+b.spec.Target.Profile)
	products := filepath.Join(derived, "Build", "Products", "Release-iphoneos")
	// a shared derived dir keeps incremental builds fast; its product must
	// not carry files (an old main.jsbundle) from an earlier build
	_ = os.RemoveAll(filepath.Join(products, p.IOS.Scheme+".app"))
	argv := []string{"xcodebuild",
		"-workspace", filepath.Join(appRoot, "ios", p.IOS.Scheme+".xcworkspace"),
		"-scheme", p.IOS.Scheme, "-configuration", "Release",
		"-destination", "generic/platform=iOS", "-derivedDataPath", derived,
		"SKIP_BUNDLING=1", "DEVELOPMENT_TEAM=" + p.IOS.Team, "-allowProvisioningUpdates"}
	if err := b.step("xcodebuild", Cmd{Argv: argv, Dir: appRoot, Env: b.env("NODE_ENV=production")}); err != nil {
		return "", provenance{}, err
	}
	app := filepath.Join(products, p.IOS.Scheme+".app")
	if !fileExists(app) {
		apps := findArtifacts(products, ".app", 1)
		if len(apps) != 1 {
			return "", provenance{}, diag(DiagBuildFailed, fmt.Sprintf("xcodebuild left %d .app products in %s", len(apps), products), "read {log}")
		}
		app = apps[0]
	}
	dst := filepath.Join(b.stage, filepath.Base(app))
	if err := b.step("copy", Cmd{Argv: []string{"ditto", app, dst}, Timeout: 5 * time.Minute, Stall: -1}); err != nil {
		return "", provenance{}, err
	}
	return dst, provenance{}, nil
}

// adapterBuild runs a project-owned build command that writes one artifact
// into {outDir}, then adopts it with its provenance file.
func (b *builder) adapterBuild(cmd []string, ext, cfgPath string) (string, provenance, error) {
	if len(cmd) == 0 {
		return "", provenance{}, diag(DiagUsage, "the adapter has no "+cfgPath+" command, which this kind needs", "add "+cfgPath+" to vybava.config.ts")
	}
	out := filepath.Join(b.stage, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		return "", provenance{}, err
	}
	argv := fillToken(cmd, "{outDir}", out)
	if err := b.step("adapter-build", Cmd{Argv: argv, Dir: b.spec.Project.RepoRoot, Env: b.env()}); err != nil {
		return "", provenance{}, err
	}
	found := findArtifacts(out, ext, 6)
	if len(found) != 1 {
		return "", provenance{}, diag(DiagAdapterCommandFailed,
			fmt.Sprintf("%s left %d %s artifacts in {outDir} (%s); it must write exactly one", cfgPath, len(found), ext, out),
			"make "+cfgPath+" write its artifact into {outDir}; log {log}")
	}
	prov := readProvenance(filepath.Dir(found[0]))
	dst := filepath.Join(b.stage, filepath.Base(found[0]))
	if err := os.Rename(found[0], dst); err != nil {
		return "", provenance{}, err
	}
	_ = os.RemoveAll(out)
	return dst, prov, nil
}

// androidGradle is the engine's Release APK recipe.
func (b *builder) androidGradle() (string, provenance, error) {
	p := b.spec.Project
	appRoot := p.appRootAbs()
	android := filepath.Join(appRoot, "android")
	if !fileExists(android) {
		if err := b.step("prebuild", Cmd{Argv: append(expoCLI(p.RepoRoot), "prebuild", "--platform", "android", "--no-install"), Dir: appRoot, Env: b.env()}); err != nil {
			return "", provenance{}, err
		}
	}
	// Gradle does not track env: drop every output that baked the old one.
	for _, d := range []string{"generated/assets", "intermediates/assets/release", "intermediates/compressed_assets/release", "outputs/apk/release"} {
		_ = os.RemoveAll(filepath.Join(android, "app", "build", filepath.FromSlash(d)))
	}
	home, err := javaHome(b.ctx, b.r)
	if err != nil {
		return "", provenance{}, err
	}
	tmp := filepath.Join(b.stage, "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return "", provenance{}, err
	}
	argv := []string{"./gradlew", ":app:assembleRelease", "-x", "lint", "-x", "lintVitalAnalyzeRelease",
		"-PreactNativeArchitectures=arm64-v8a", "--no-daemon", "-Dorg.gradle.jvmargs=-Xmx6g -XX:MaxMetaspaceSize=1g"}
	env := b.env("JAVA_HOME="+home, "PATH="+filepath.Join(home, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"),
		"NODE_ENV=production", "TMPDIR="+tmp)
	if err := b.step("gradle", Cmd{Argv: argv, Dir: android, Env: env}); err != nil {
		return "", provenance{}, err
	}
	_ = os.RemoveAll(tmp)
	apk := filepath.Join(android, "app", "build", "outputs", "apk", "release", "app-release.apk")
	if !fileExists(apk) {
		return "", provenance{}, diag(DiagBuildFailed, "gradle succeeded but left no "+apk, "read {log}")
	}
	dst := filepath.Join(b.stage, "app-release.apk")
	if err := copyFile(apk, dst); err != nil {
		return "", provenance{}, err
	}
	assets, err := assetDigests(filepath.Join(android, "app", "build", "generated", "res", "react", "release"))
	if err != nil {
		return "", provenance{}, err
	}
	return dst, provenance{AndroidAssets: assets}, nil
}

func fillToken(argv []string, token, value string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		out[i] = strings.ReplaceAll(a, token, value)
	}
	return out
}

// findArtifacts lists files or dirs ending in ext under root, not inside
// another match (an .app's PlugIns), up to depth levels down.
func findArtifacts(root, ext string, depth int) []string {
	var out []string
	base := strings.Count(filepath.Clean(root), string(os.PathSeparator))
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if strings.Count(path, string(os.PathSeparator))-base > depth {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if path != root && strings.HasSuffix(d.Name(), ext) {
			out = append(out, path)
			if d.IsDir() {
				return filepath.SkipDir
			}
		}
		return nil
	})
	return out
}

// readProvenance reads `*build-metadata.json` beside an adopted artifact:
// productionEquivalent at the top level or under instrumentation, and
// publicEnvHash.
func readProvenance(dir string) provenance {
	matches, _ := filepath.Glob(filepath.Join(dir, "*build-metadata.json"))
	var prov provenance
	for _, m := range matches {
		var doc struct {
			ProductionEquivalent *bool  `json:"productionEquivalent"`
			PublicEnvHash        string `json:"publicEnvHash"`
			Instrumentation      struct {
				ProductionEquivalent *bool `json:"productionEquivalent"`
			} `json:"instrumentation"`
		}
		b, err := os.ReadFile(m)
		if err != nil || json.Unmarshal(b, &doc) != nil {
			continue
		}
		if doc.ProductionEquivalent != nil {
			prov.ProductionEquivalent = *doc.ProductionEquivalent
		}
		if doc.Instrumentation.ProductionEquivalent != nil {
			prov.ProductionEquivalent = *doc.Instrumentation.ProductionEquivalent
		}
		prov.PublicEnvHash = doc.PublicEnvHash
	}
	return prov
}

// ImportSpec is `perflab build import <artifact>`.
type ImportSpec struct {
	Project  Project
	Target   Target
	Env      ProfileEnv
	Artifact string
	Progress *Progress
}

// Import adopts an existing .app/.apk built from this worktree: the key is
// recomputed with the artifact's own signing (and, on iOS, its Xcode
// build), and the entry is stored with provenance imported=true.
func (s Store) Import(ctx context.Context, r Runner, spec ImportSpec) (BuildResult, error) {
	t := spec.Target
	if err := t.validate(); err != nil {
		return BuildResult{}, err
	}
	src, err := filepath.Abs(spec.Artifact)
	if err != nil || !fileExists(src) {
		return BuildResult{}, diag(DiagUsage, "no artifact at "+spec.Artifact, "perflab build import <path to .app|.apk> --platform ... --profile ... --kind ...")
	}
	wantExt := map[string]string{"ios": ".app", "android": ".apk"}[t.Platform]
	if !strings.HasSuffix(strings.TrimSuffix(src, "/"), wantExt) {
		return BuildResult{}, diag(DiagUsage, fmt.Sprintf("a %s build imports a %s, not %s", t.Platform, wantExt, filepath.Base(src)), "pass the "+wantExt+" path")
	}
	phase(spec.Progress, "read-artifact")
	var signing Signing
	var toolchain Toolchain
	var version string // the artifact's marketing version
	if t.Platform == "ios" {
		info, err := iosAppInfo(ctx, r, src)
		if err != nil {
			return BuildResult{}, err
		}
		if err := importAppID(spec.Project, t, info.BundleID); err != nil {
			return BuildResult{}, err
		}
		version = info.Version
		if signing, err = iosSigningOf(ctx, r, src, s.Dirs.Cache); err != nil {
			return BuildResult{}, err
		}
		toolchain = Toolchain{Xcode: info.XcodeBuild}
	} else {
		info, err := apkInfo(ctx, r, src)
		if err != nil {
			return BuildResult{}, err
		}
		if err := importAppID(spec.Project, t, info.Package); err != nil {
			return BuildResult{}, err
		}
		version = info.VersionName
		signing = Signing{KeystoreCertSHA256: info.CertSHA256}
		if toolchain, err = ResolveToolchain(ctx, r, "android"); err != nil {
			return BuildResult{}, err
		}
	}
	phase(spec.Progress, "fingerprint")
	fp, err := RunFingerprint(ctx, r, FingerprintSpec{Project: spec.Project, Target: t, Env: spec.Env.Vars, Signing: &signing, Toolchain: &toolchain})
	if err != nil {
		return BuildResult{}, err
	}
	if fp.AppVersion != "" && version != "" && version != fp.AppVersion {
		// The key is recomputed from THIS checkout, so an artifact of another
		// app version was built from other native inputs: adopting it would
		// file a binary under a key that does not describe it.
		return BuildResult{}, diag(DiagFingerprintMismatch,
			fmt.Sprintf("the artifact is version %s, this checkout's app config says %s: it was built from other native inputs than key %s", version, fp.AppVersion, fp.Key),
			fmt.Sprintf("perflab build native --platform %s --profile %s --kind %s --json", t.Platform, t.Profile, t.Kind))
	}
	if b, err := s.FindBuild(t, fp.Key); err == nil {
		return BuildResult{Hit: true, Build: b, Fingerprint: fp, Next: nextAfterBuild(b)}, nil
	}
	st, err := newStage(s.buildDir(t, fp.Key))
	if err != nil {
		return BuildResult{}, err
	}
	dst := filepath.Join(st.Dir, filepath.Base(strings.TrimSuffix(src, "/")))
	phase(spec.Progress, "copy")
	if t.Platform == "ios" {
		res, err := run(ctx, r, Cmd{Argv: []string{"ditto", src, dst}, Timeout: 5 * time.Minute})
		if err == nil && res.Exit != 0 {
			err = fmt.Errorf("ditto %s exited %d: %s", src, res.Exit, lastLines(res.Stderr, 2))
		}
		if err != nil {
			st.discard()
			return BuildResult{}, err
		}
	} else if err := copyFile(src, dst); err != nil {
		st.discard()
		return BuildResult{}, err
	}
	m, err := describeArtifact(ctx, r, spec.Project, t, fp, dst, st.Dir)
	if err != nil {
		st.discard()
		return BuildResult{}, err
	}
	prov := readProvenance(filepath.Dir(src))
	m.Imported, m.ImportedFrom = true, src
	m.ProductionEquivalent = prov.ProductionEquivalent
	m.PublicEnvHash, m.EnvNames = spec.Env.Hash(), spec.Env.Names()
	if prov.PublicEnvHash != "" {
		m.PublicEnvHash = prov.PublicEnvHash
	}
	if m.Source, err = ReadSourceIdentity(ctx, r, spec.Project.RepoRoot, spec.Project.ID); err != nil {
		st.discard()
		return BuildResult{}, err
	}
	return s.commitBuild(st, m, fp)
}

// importAppID refuses an artifact of another app before anything is copied:
// a store build beside the adapter's dev variant (another package id) is the
// usual mix-up, and the way out is building the adapter's app once.
func importAppID(p Project, t Target, got string) error {
	want := p.appID(t.Platform)
	if want == "" || got == want {
		return nil
	}
	return diag(DiagBuildFailed, fmt.Sprintf("the artifact is %s, the adapter's app (perflab.app.%s in vybava.config.ts) is %s: importing it would measure another app", got, t.Platform, want),
		fmt.Sprintf("perflab build native --platform %s --profile %s --kind %s --json", t.Platform, t.Profile, t.Kind))
}

// describeArtifact reads the app id, version and actual signing of a
// built artifact and refuses one the key does not describe.
func describeArtifact(ctx context.Context, r Runner, p Project, t Target, fp Fingerprint, artifact, entryDir string) (BuildManifest, error) {
	m := BuildManifest{
		Key: fp.Key, SourceKey: fp.SourceKey, ExpoHash: fp.ExpoHash,
		Platform: t.Platform, Profile: t.Profile, Kind: t.Kind,
		Signing: fp.Signing, Toolchain: fp.Toolchain,
		CreatedAt: time.Now().UTC(), Artifact: filepath.Base(artifact), EnvNames: []string{},
	}
	if t.Platform == "ios" {
		info, err := iosAppInfo(ctx, r, artifact)
		if err != nil {
			return m, err
		}
		m.AppID, m.Version, m.BuildNumber = info.BundleID, info.Version, info.BuildNumber
		actual, err := iosSigningOf(ctx, r, artifact, entryDir)
		if err != nil {
			return m, err
		}
		if fp.Signing.IdentitySHA1 != "" && actual.IdentitySHA1 != fp.Signing.IdentitySHA1 {
			return m, diag(DiagBuildFailed,
				fmt.Sprintf("Xcode signed the app with identity %s, the key assumed %s", actual.IdentitySHA1, fp.Signing.IdentitySHA1),
				"security find-identity -v -p codesigning: keep one Apple Development identity for the team, then re-run")
		}
	} else {
		info, err := apkInfo(ctx, r, artifact)
		if err != nil {
			return m, err
		}
		m.AppID, m.Version, m.BuildNumber = info.Package, info.VersionName, info.VersionCode
		if fp.Signing.KeystoreCertSHA256 != "" && info.CertSHA256 != fp.Signing.KeystoreCertSHA256 {
			return m, diag(DiagBuildFailed,
				fmt.Sprintf("the APK is signed by certificate %s, the key assumed %s", info.CertSHA256, fp.Signing.KeystoreCertSHA256),
				"build with the template debug keystore (android/app/debug.keystore), or sign through the adapter's build.android")
		}
		sum, err := fileSHA256(artifact)
		if err != nil {
			return m, err
		}
		m.ArtifactSHA256 = sum
		if info.CertSHA256 == TemplateDebugKeystoreSHA256 {
			ks := filepath.Join(p.appRootAbs(), "android", "app", "debug.keystore")
			if fileExists(ks) {
				if err := copyFile(ks, filepath.Join(entryDir, "debug.keystore")); err == nil {
					m.Keystore = "debug.keystore"
				}
			}
		}
	}
	if want := p.appID(t.Platform); want != "" && m.AppID != want {
		return m, diag(DiagBuildFailed, fmt.Sprintf("the artifact is %s, the adapter's app is %s", m.AppID, want),
			"build the adapter's app (perflab.app in vybava.config.ts), or fix the app id there")
	}
	return m, nil
}

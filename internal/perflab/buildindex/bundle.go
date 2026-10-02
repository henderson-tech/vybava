package buildindex

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// hbcMagic opens every Hermes bytecode file; the uint32 after it is the
// bytecode version the native Hermes must match.
var hbcMagic = []byte{0xc6, 0x1f, 0xbc, 0x03, 0xc1, 0x03, 0x19, 0x1f}

// bundleFile is the embedded bundle's name inside the native app.
func bundleFile(platform string) string {
	if platform == "ios" {
		return "main.jsbundle"
	}
	return "index.android.bundle"
}

// BundleSpec is one `perflab bundle export`.
type BundleSpec struct {
	Project  Project
	Platform string
	Profile  string
	Env      ProfileEnv
	// Ref exports from the detached source worktree at that git ref.
	Ref      string
	Label    string
	Timeout  time.Duration
	Progress *Progress
}

// BundleResult is the `bundle export` payload.
type BundleResult struct {
	Hit    bool        `json:"hit"`
	Bundle Bundle      `json:"bundle"`
	Source *SourceTree `json:"source,omitempty"`
	Next   []string    `json:"-"`
}

var modulesCount = regexp.MustCompile(`\((\d+) modules\)`)

// ExportBundle runs `expo export:embed` (Hermes bytecode, minified, no dev)
// under the profile env with EXPO_NO_DOTENV=1, a fresh TMPDIR and
// --reset-cache, so Metro cannot replay old EXPO_PUBLIC_* literals; checks
// the Hermes magic and stores the bundle content-addressed. Exporting an
// already stored bundle answers the stored entry.
func (s Store) ExportBundle(ctx context.Context, r Runner, spec BundleSpec) (BundleResult, error) {
	kind := KindShell
	if spec.Platform == "android" {
		kind = KindBundled
	}
	t := Target{Platform: spec.Platform, Profile: spec.Profile, Kind: kind}
	if err := t.validate(); err != nil {
		return BundleResult{}, err
	}
	p := spec.Project
	var src *SourceTree
	if spec.Ref != "" {
		tree, err := EnsureSourceWorktree(ctx, r, p.RepoRoot, spec.Ref, spec.Progress)
		if err != nil {
			return BundleResult{}, err
		}
		p.RepoRoot, src = tree.Dir, &tree
	}
	phase(spec.Progress, "fingerprint")
	fp, err := RunFingerprint(ctx, r, FingerprintSpec{Project: p, Target: t, Env: spec.Env.Vars, Signing: &Signing{}, Toolchain: &Toolchain{}})
	if err != nil {
		return BundleResult{}, err
	}
	st, err := newStage(filepath.Join(s.Dirs.bundlesDir(), "pending"))
	if err != nil {
		return BundleResult{}, err
	}
	tmp := filepath.Join(st.Dir, "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		st.discard()
		return BundleResult{}, err
	}
	logFile, err := os.Create(filepath.Join(st.Dir, "export.log"))
	if err != nil {
		st.discard()
		return BundleResult{}, err
	}
	out := filepath.Join(st.Dir, bundleFile(spec.Platform))
	js := filepath.Join(st.Dir, hermesInput)
	entry := p.Entry
	if entry == "" {
		entry = "index.js"
	}
	// Expo's --bytecode path compiles the JS from a random temp dir and
	// hermesc embeds that path, so two exports of one tree never share a
	// sha. This is the same pipeline made deterministic: with bytecode on
	// Expo skips JS minification (Hermes minifies), so export unminified JS
	// and run the project's hermesc with Expo's flags on a fixed name.
	argv := append(expoCLI(p.RepoRoot), "export:embed",
		"--platform", spec.Platform, "--dev", "false", "--minify", "false", "--bytecode", "false",
		"--entry-file", entry, "--bundle-output", js, "--assets-dest", filepath.Join(st.Dir, "assets"), "--reset-cache")
	timeout := spec.Timeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	phase(spec.Progress, "export")
	stopBeat := heartbeat(spec.Progress, 30*time.Second)
	started := time.Now()
	fmt.Fprintf(logFile, "$ (cd %s && %s)\n", p.appRootAbs(), strings.Join(argv, " "))
	res, err := run(ctx, r, Cmd{
		Argv:    argv,
		Dir:     p.appRootAbs(),
		Env:     append([]string{"EXPO_NO_DOTENV=1", "CI=1", "NODE_ENV=production", "TMPDIR=" + tmp}, spec.Env.Vars...),
		Log:     logFile,
		Timeout: timeout,
		Stall:   5 * time.Minute,
	})
	stopBeat()
	_ = os.RemoveAll(tmp)
	if err == nil && res.Exit == 0 && !res.Stalled && !res.TimedOut && fileExists(js) {
		phase(spec.Progress, "hermesc")
		err = s.compileHermes(ctx, r, p, st.Dir, out, logFile)
	} else if err == nil {
		err = diag(DiagBuildFailed, fmt.Sprintf("expo export:embed exited %d: %s (log {log})", res.Exit, lastLines(res.Stdout, 3)),
			"read the log, fix the JS build, re-run perflab bundle export")
	}
	_ = logFile.Close()
	_ = os.Remove(js)
	if err != nil {
		kept := s.keepFailedLog(filepath.Join(st.Dir, "export.log"), "bundle-"+spec.Platform)
		st.discard()
		return BundleResult{}, withLog(err, kept)
	}
	version, err := checkHBC(out)
	if err != nil {
		st.discard()
		return BundleResult{}, err
	}
	sum, err := fileSHA256(out)
	if err != nil {
		st.discard()
		return BundleResult{}, err
	}
	count, digest, err := assetsDigest(filepath.Join(st.Dir, "assets"))
	if err != nil {
		st.discard()
		return BundleResult{}, err
	}
	m := BundleManifest{
		SHA256: sum, Platform: spec.Platform, Profile: spec.Profile, Label: spec.Label,
		SourceKey: fp.SourceKey, Ref: spec.Ref,
		PublicEnvHash: spec.Env.Hash(), EnvNames: spec.Env.Names(),
		AssetsCount: count, AssetsDigest: digest, HBC: true, HBCVersion: version,
		File: bundleFile(spec.Platform), CreatedAt: time.Now().UTC(), DurationMs: time.Since(started).Milliseconds(),
	}
	if m.EnvNames == nil {
		m.EnvNames = []string{}
	}
	if mm := modulesCount.FindSubmatch(res.Stdout); mm != nil {
		m.Modules, _ = strconv.Atoi(string(mm[1]))
	}
	if m.Source, err = ReadSourceIdentity(ctx, r, p.RepoRoot, p.ID); err != nil {
		st.discard()
		return BundleResult{}, err
	}
	if err := writeJSONAtomic(filepath.Join(st.Dir, manifestFile), m); err != nil {
		st.discard()
		return BundleResult{}, err
	}
	st.final = filepath.Join(s.Dirs.bundlesDir(), sum[:16])
	existed, err := st.commit()
	if err != nil {
		return BundleResult{}, err
	}
	b, err := s.FindBundle(sum)
	if err != nil {
		return BundleResult{}, err
	}
	return BundleResult{Hit: existed, Bundle: b, Source: src, Next: []string{
		fmt.Sprintf("perflab build find --platform %s --profile %s --json", spec.Platform, spec.Profile),
		fmt.Sprintf("perflab pack --native <key> --bundle %s --json", sum[:16]),
	}}, nil
}

// checkHBC refuses a bundle that is not Hermes bytecode and returns its
// bytecode version.
func checkHBC(path string) (uint32, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	head := make([]byte, 12)
	if _, err := io.ReadFull(f, head); err != nil || !bytes.Equal(head[:8], hbcMagic) {
		return 0, diag(DiagBundleNotHBC, filepath.Base(path)+" is not Hermes bytecode (no c6 1f bc 03 c1 03 19 1f magic)",
			"re-export with --bytecode (perflab bundle export does), and check the app runs Hermes")
	}
	return binary.LittleEndian.Uint32(head[8:12]), nil
}

// hbcVersionOf reads the bytecode version of a Hermes file, 0 if none.
func hbcVersionOf(b []byte) uint32 {
	if len(b) < 12 || !bytes.Equal(b[:8], hbcMagic) {
		return 0
	}
	return binary.LittleEndian.Uint32(b[8:12])
}

// assetDigests maps every file under dir (relative, slash paths) to its
// sha256; a missing dir is an empty map.
func assetDigests(dir string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			if os.IsNotExist(err) && path == dir {
				return filepath.SkipDir
			}
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(dir, path)
		sum, err := fileSHA256(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = sum
		return nil
	})
	return out, err
}

// assetsDigest counts the exported assets and hashes their names and
// contents (sorted `rel\tsha256` lines).
func assetsDigest(dir string) (int, string, error) {
	sums, err := assetDigests(dir)
	if err != nil {
		return 0, "", err
	}
	lines := make([]string, 0, len(sums))
	for rel, sum := range sums {
		lines = append(lines, rel+"\t"+sum)
	}
	sort.Strings(lines)
	return len(lines), sha256Hex([]byte(strings.Join(lines, "\n")))[:16], nil
}

// hermesInput is the fixed name the plain JS is compiled from; hermesc
// embeds it, so it must not vary between exports.
const hermesInput = "index.js"

// compileHermes runs the project's hermesc as Expo's serializer does
// (-emit-binary -O), from dir on the relative hermesInput.
func (s Store) compileHermes(ctx context.Context, r Runner, p Project, dir, out string, log io.Writer) error {
	hermesc, err := projectHermesc(ctx, r, p)
	if err != nil {
		return err
	}
	argv := []string{hermesc, "-emit-binary", "-out", out, hermesInput, "-O"}
	fmt.Fprintf(log, "\n$ (cd %s && %s)\n", dir, strings.Join(argv, " "))
	res, err := run(ctx, r, Cmd{Argv: argv, Dir: dir, Log: log, Timeout: 10 * time.Minute})
	if err != nil {
		return err
	}
	if res.Exit != 0 || !fileExists(out) {
		return diag(DiagBuildFailed, fmt.Sprintf("hermesc exited %d: %s (log {log})", res.Exit, lastLines(res.Stdout, 3)),
			"read the log; the JS bundle does not compile to Hermes bytecode")
	}
	return nil
}

// resolveRN prints the react-native root and the hermes-compiler root
// (empty when absent) as the app resolves them, like Expo's resolve-from.
const resolveRN = `const p=require("path");const rn=p.dirname(require.resolve("react-native/package.json",{paths:[process.argv[1]]}));let hc="";try{hc=p.dirname(require.resolve("hermes-compiler/package.json",{paths:[rn]}))}catch(e){}console.log(rn);console.log(hc)`

// projectHermesc finds the hermesc Expo would use for this app:
// REACT_NATIVE_OVERRIDE_HERMES_DIR, a from-source build, the
// hermes-compiler package (RN 0.83+), then react-native/sdks/hermesc.
func projectHermesc(ctx context.Context, r Runner, p Project) (string, error) {
	res, err := run(ctx, r, Cmd{Argv: []string{"node", "-e", resolveRN, p.appRootAbs()}, Dir: p.appRootAbs(), Timeout: time.Minute})
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(string(res.Stdout)), "\n")
	if res.Exit != 0 || len(lines) == 0 || lines[0] == "" {
		return "", diag(DiagBuildFailed, "react-native does not resolve from "+p.appRootAbs()+": "+lastLines(res.Stderr, 2),
			"bun install (the app's dependencies), then re-run perflab bundle export")
	}
	rn, hc := lines[0], ""
	if len(lines) > 1 {
		hc = strings.TrimSpace(lines[1])
	}
	plat := map[string]string{"darwin": "osx-bin/hermesc", "linux": "linux64-bin/hermesc", "windows": "win64-bin/hermesc.exe"}[runtime.GOOS]
	var candidates []string
	if dir := os.Getenv("REACT_NATIVE_OVERRIDE_HERMES_DIR"); dir != "" {
		candidates = append(candidates, filepath.Join(dir, "build", "bin", "hermesc"))
	}
	candidates = append(candidates, filepath.Join(rn, "ReactAndroid", "hermes-engine", "build", "hermes", "bin", "hermesc"))
	if hc != "" {
		candidates = append(candidates, filepath.Join(hc, "hermesc", filepath.FromSlash(plat)))
	}
	candidates = append(candidates, filepath.Join(rn, "sdks", "hermesc", filepath.FromSlash(plat)))
	for _, c := range candidates {
		if fileExists(c) {
			return c, nil
		}
	}
	return "", diag(DiagToolMissing, "no hermesc for this app (looked in "+strings.Join(candidates, ", ")+")",
		"bun install (react-native ships hermesc in hermes-compiler or sdks/hermesc)")
}

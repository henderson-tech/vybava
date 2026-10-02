package buildindex

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
)

// Project is what this package needs from the adapter's `perflab` section,
// already resolved by the verb layer: every command is argv with its
// {tokens} filled, except {outDir}, which this package fills per build.
type Project struct {
	// ID names the project in provenance (the git remote's owner/repo).
	ID string
	// RepoRoot is the absolute root of the worktree perflab runs in.
	RepoRoot string
	// AppRoot is the Expo app, relative to RepoRoot (`apps/client`).
	AppRoot string
	// Entry is the JS entry file, relative to AppRoot (`index.js`).
	Entry   string
	IOS     IOSApp
	Android AndroidApp
	// FingerprintCmd overrides the default fingerprint command.
	FingerprintCmd []string
	// ExtraNativeInputs are `<file>#<json pointer>` refs under AppRoot.
	ExtraNativeInputs []string
	// IOSBundledCmd builds the as-shipped iOS app into {outDir}.
	IOSBundledCmd []string
	// AndroidBuildCmd replaces the engine's Gradle recipe; it writes one
	// APK into {outDir}.
	AndroidBuildCmd []string
}

// IOSApp is the adapter's app.ios.
type IOSApp struct {
	Scheme   string
	BundleID string
	Team     string
}

// AndroidApp is the adapter's app.android.
type AndroidApp struct {
	Package  string
	Activity string
}

// Target is one build identity: platform, profile and kind.
type Target struct {
	Platform string
	Profile  string
	Kind     string
}

// Kinds of native builds: a shell skips JS bundling and needs pack; a
// bundled build is the as-shipped Release.
const (
	KindShell   = "shell"
	KindBundled = "bundled"
)

func (t Target) validate() error {
	if t.Platform != "ios" && t.Platform != "android" {
		return diag(DiagUsage, fmt.Sprintf("platform %q is not ios or android", t.Platform), "pass --platform ios|android")
	}
	if t.Profile == "" {
		return diag(DiagUsage, "no profile named", "pass --profile <name> (a key of perflab.profiles in vybava.config.ts)")
	}
	if t.Kind != KindShell && t.Kind != KindBundled {
		return diag(DiagUsage, fmt.Sprintf("kind %q is not shell or bundled", t.Kind), "pass --kind shell|bundled")
	}
	if t.Platform == "android" && t.Kind == KindShell {
		return diag(DiagUsage, "android builds always embed their JS (Gradle bundles it), so there is no android shell kind",
			"pass --kind bundled for android")
	}
	return nil
}

func (p Project) appRootAbs() string { return filepath.Join(p.RepoRoot, p.AppRoot) }

// appID is the platform's bundle id or package.
func (p Project) appID(platform string) string {
	if platform == "ios" {
		return p.IOS.BundleID
	}
	return p.Android.Package
}

// expoCLI is the package runner for the expo CLI, by lockfile.
func expoCLI(repoRoot string) []string {
	for _, f := range []string{"bun.lock", "bun.lockb"} {
		if fileExists(filepath.Join(repoRoot, f)) {
			return []string{"bunx", "expo"}
		}
	}
	return []string{"npx", "expo"}
}

// installCmd is the frozen dependency install for a source checkout.
func installCmd(root string) []string {
	switch {
	case fileExists(filepath.Join(root, "bun.lock")) || fileExists(filepath.Join(root, "bun.lockb")):
		return []string{"bun", "install", "--frozen-lockfile"}
	case fileExists(filepath.Join(root, "pnpm-lock.yaml")):
		return []string{"pnpm", "install", "--frozen-lockfile"}
	case fileExists(filepath.Join(root, "yarn.lock")):
		return []string{"yarn", "install", "--frozen-lockfile"}
	default:
		return []string{"npm", "ci"}
	}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// ProfileEnv is a profile's environment: the KEY=VALUE lines the adapter's
// profiles.<name>.env command printed. Envelopes carry only Names and Hash;
// values never leave the child processes.
type ProfileEnv struct {
	Vars []string
}

// ParseProfileEnv reads KEY=VALUE lines (blank lines, # comments and an
// `export ` prefix are tolerated).
func ParseProfileEnv(out []byte) (ProfileEnv, error) {
	var vars []string
	seen := map[string]int{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok || !envName.MatchString(k) {
			return ProfileEnv{}, diag(DiagAdapterCommandFailed,
				fmt.Sprintf("the profile env command printed a line that is not KEY=VALUE (name %q)", truncate(k, 40)),
				"make perflab.profiles.<name>.env print only KEY=VALUE lines")
		}
		v = strings.Trim(v, `"'`)
		if i, dup := seen[k]; dup {
			vars[i] = k + "=" + v
			continue
		}
		seen[k] = len(vars)
		vars = append(vars, k+"="+v)
	}
	sort.Strings(vars)
	return ProfileEnv{Vars: vars}, nil
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Names are the variable names, sorted.
func (e ProfileEnv) Names() []string { return envNames(e.Vars) }

// Hash is publicEnvHash: the first 16 hex of sha256 over the sorted lines.
func (e ProfileEnv) Hash() string {
	if len(e.Vars) == 0 {
		return ""
	}
	return sha256Hex([]byte(strings.Join(e.Vars, "\n")))[:16]
}

// LoadProfileEnv runs the adapter's profile env command in dir.
func LoadProfileEnv(ctx context.Context, r Runner, argv []string, dir string) (ProfileEnv, error) {
	if len(argv) == 0 {
		return ProfileEnv{}, nil
	}
	res, err := run(ctx, r, Cmd{Argv: argv, Dir: dir, Env: []string{"EXPO_NO_DOTENV=1"}})
	if err != nil {
		return ProfileEnv{}, err
	}
	if res.Exit != 0 {
		return ProfileEnv{}, diag(DiagAdapterCommandFailed,
			fmt.Sprintf("the profile env command exited %d: %s", res.Exit, lastLines(res.Stderr, 3)),
			"run it by hand from "+dir+": "+strings.Join(argv, " "))
	}
	return ParseProfileEnv(res.Stdout)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// TemplateDebugKeystoreSHA256 is the certificate of React Native's template
// android/app/debug.keystore (alias androiddebugkey, password android):
// public, identical in every prebuilt RN project.
const TemplateDebugKeystoreSHA256 = "fac61745dc0903786fb9ede62a962b399f7348f0bb6f899b8332667591033b9c"

// ResolveSigning is the host's signing input for a key: the iOS team plus
// its Apple Development identity, or the Android keystore certificate.
func ResolveSigning(ctx context.Context, r Runner, p Project, platform string) (Signing, error) {
	if platform == "android" {
		ks := filepath.Join(p.appRootAbs(), "android", "app", "debug.keystore")
		if !fileExists(ks) {
			return Signing{KeystoreCertSHA256: TemplateDebugKeystoreSHA256}, nil
		}
		sum, err := keystoreCertSHA256(ctx, r, ks)
		if err != nil {
			return Signing{}, err
		}
		return Signing{KeystoreCertSHA256: sum}, nil
	}
	if p.IOS.Team == "" {
		return Signing{}, diag(DiagUsage, "the adapter names no iOS team", "set perflab.app.ios.team in vybava.config.ts, or pass --team <id>")
	}
	ids, err := codesigningIdentities(ctx, r)
	if err != nil {
		return Signing{}, err
	}
	certs, err := keychainCertTeams(ctx, r)
	if err != nil {
		return Signing{}, err
	}
	var best []identity
	for _, id := range ids {
		if certs[id.SHA1] == p.IOS.Team && isDevelopmentIdentity(id.Name) {
			best = append(best, id)
		}
	}
	if len(best) == 0 {
		return Signing{}, diag(DiagSigningIdentityMissing,
			"no valid Apple Development identity for team "+p.IOS.Team+" in the keychain",
			"security find-identity -v -p codesigning (add the team's account in Xcode > Settings > Accounts)")
	}
	sort.Slice(best, func(i, j int) bool { return best[i].SHA1 < best[j].SHA1 })
	return Signing{Team: p.IOS.Team, IdentitySHA1: best[0].SHA1}, nil
}

func isDevelopmentIdentity(name string) bool {
	return strings.HasPrefix(name, "Apple Development:") || strings.HasPrefix(name, "iPhone Developer:")
}

type identity struct {
	SHA1 string
	Name string
}

var identityLine = regexp.MustCompile(`^\s*\d+\)\s+([0-9A-F]{40})\s+"(.*)"`)

// codesigningIdentities lists `security find-identity -v -p codesigning`.
func codesigningIdentities(ctx context.Context, r Runner) ([]identity, error) {
	res, err := run(ctx, r, Cmd{Argv: []string{"security", "find-identity", "-v", "-p", "codesigning"}})
	if err != nil {
		return nil, err
	}
	var out []identity
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		if m := identityLine.FindStringSubmatch(line); m != nil {
			out = append(out, identity{SHA1: m[1], Name: m[2]})
		}
	}
	return out, nil
}

// keychainCertTeams maps every keychain certificate's SHA-1 to its subject
// OU (the Apple team id). Certificates are public; no key is read.
func keychainCertTeams(ctx context.Context, r Runner) (map[string]string, error) {
	res, err := run(ctx, r, Cmd{Argv: []string{"security", "find-certificate", "-a", "-p"}})
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	rest := res.Stdout
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil || len(cert.Subject.OrganizationalUnit) == 0 {
			continue
		}
		sum := sha1.Sum(block.Bytes)
		out[strings.ToUpper(hex.EncodeToString(sum[:]))] = cert.Subject.OrganizationalUnit[0]
	}
	return out, nil
}

var keytoolSHA256 = regexp.MustCompile(`SHA256:\s*([0-9A-Fa-f:]{95})`)

// keystoreCertSHA256 reads the template debug keystore's certificate.
func keystoreCertSHA256(ctx context.Context, r Runner, keystore string) (string, error) {
	res, err := run(ctx, r, Cmd{Argv: []string{"keytool", "-list", "-v", "-keystore", keystore, "-storepass", "android", "-alias", "androiddebugkey"}})
	if err != nil {
		return "", err
	}
	m := keytoolSHA256.FindSubmatch(res.Stdout)
	if res.Exit != 0 || m == nil {
		return "", diag(DiagSigningIdentityMissing,
			keystore+" is not the template debug keystore (alias androiddebugkey, password android)",
			"restore it with `expo prebuild --platform android --clean`, or sign through the adapter's build.android command")
	}
	return strings.ToLower(strings.ReplaceAll(string(m[1]), ":", "")), nil
}

var (
	xcodeBuildVersion = regexp.MustCompile(`Build version (\S+)`)
	javaVersion       = regexp.MustCompile(`version "(\d+)`)
)

// ResolveToolchain reads the Xcode build version (iOS) or the JDK major
// the Gradle recipe runs (Android).
func ResolveToolchain(ctx context.Context, r Runner, platform string) (Toolchain, error) {
	if platform == "ios" {
		res, err := run(ctx, r, Cmd{Argv: []string{"xcodebuild", "-version"}})
		if err != nil {
			return Toolchain{}, err
		}
		m := xcodeBuildVersion.FindSubmatch(res.Stdout)
		if res.Exit != 0 || m == nil {
			return Toolchain{}, diag(DiagToolMissing, "xcodebuild -version printed no build version: "+lastLines(res.Stderr, 2),
				"sudo xcode-select -s /Applications/Xcode.app")
		}
		return Toolchain{Xcode: string(m[1])}, nil
	}
	home, err := javaHome(ctx, r)
	if err != nil {
		return Toolchain{}, err
	}
	res, err := run(ctx, r, Cmd{Argv: []string{filepath.Join(home, "bin", "java"), "-version"}})
	if err != nil {
		return Toolchain{}, err
	}
	m := javaVersion.FindSubmatch(append(res.Stderr, res.Stdout...))
	if m == nil {
		return Toolchain{}, diag(DiagToolMissing, "java -version under "+home+" printed no version", "brew install openjdk@21")
	}
	return Toolchain{JDK: string(m[1])}, nil
}

// jdkMajor is the JDK the Android recipe pins (Gradle and AGP of RN 0.8x).
const jdkMajor = "21"

// javaHome resolves JDK 21: /usr/libexec/java_home on macOS, else JAVA_HOME.
func javaHome(ctx context.Context, r Runner) (string, error) {
	if runtime.GOOS == "darwin" {
		res, err := run(ctx, r, Cmd{Argv: []string{"/usr/libexec/java_home", "-v", jdkMajor}})
		if err == nil && res.Exit == 0 {
			if home := strings.TrimSpace(string(res.Stdout)); home != "" {
				return home, nil
			}
		}
		return "", diag(DiagToolMissing, "no JDK "+jdkMajor+" is installed (Gradle for React Native needs it)", "brew install openjdk@"+jdkMajor)
	}
	if home := os.Getenv("JAVA_HOME"); home != "" {
		return home, nil
	}
	return "", diag(DiagToolMissing, "JAVA_HOME is unset", "install JDK "+jdkMajor+" and export JAVA_HOME")
}

// SourceIdentity is the provenance of a build or bundle's source tree.
type SourceIdentity struct {
	Project  string `json:"project,omitempty"`
	Worktree string `json:"worktree"`
	Commit   string `json:"commit"`
	Dirty    bool   `json:"dirty"`
	DiffHash string `json:"diffHash,omitempty"`
}

// ReadSourceIdentity is HEAD, dirtiness and a hash of the diff plus the
// untracked files (the same recipe as FixIt's build metadata).
func ReadSourceIdentity(ctx context.Context, r Runner, root, project string) (SourceIdentity, error) {
	git := func(args ...string) ([]byte, error) {
		res, err := run(ctx, r, Cmd{Argv: append([]string{"git", "-C", root}, args...)})
		if err != nil {
			return nil, err
		}
		if res.Exit != 0 {
			return nil, fmt.Errorf("git %s in %s exited %d: %s", strings.Join(args, " "), root, res.Exit, lastLines(res.Stderr, 2))
		}
		return res.Stdout, nil
	}
	head, err := git("rev-parse", "HEAD")
	if err != nil {
		return SourceIdentity{}, err
	}
	id := SourceIdentity{Project: project, Worktree: filepath.Base(root), Commit: strings.TrimSpace(string(head))}
	status, err := git("status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return SourceIdentity{}, err
	}
	if len(bytes.TrimSpace(status)) == 0 {
		return id, nil
	}
	id.Dirty = true
	diff, err := git("diff", "--binary", "HEAD")
	if err != nil {
		return SourceIdentity{}, err
	}
	h := sha256.New()
	h.Write(diff)
	untracked, err := git("ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return SourceIdentity{}, err
	}
	names := strings.Split(strings.TrimRight(string(untracked), "\x00"), "\x00")
	sort.Strings(names)
	for _, n := range names {
		if n == "" {
			continue
		}
		h.Write([]byte("\x00" + n + "\x00"))
		if b, err := os.ReadFile(filepath.Join(root, n)); err == nil {
			h.Write(b)
		}
	}
	id.DiffHash = hex.EncodeToString(h.Sum(nil))
	return id, nil
}

// invocation is the exact `perflab build native` for this target.
func (t Target) invocation() string {
	return fmt.Sprintf("perflab build native --platform %s --profile %s --kind %s --json", t.Platform, t.Profile, t.Kind)
}

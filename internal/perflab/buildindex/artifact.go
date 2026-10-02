package buildindex

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// iosInfo is the part of an .app's Info.plist the index records.
type iosInfo struct {
	BundleID    string
	Version     string
	BuildNumber string
	XcodeBuild  string
}

func plistJSON(ctx context.Context, r Runner, plist string) (map[string]any, error) {
	res, err := run(ctx, r, Cmd{Argv: []string{"plutil", "-convert", "json", "-o", "-", plist}})
	if err != nil {
		return nil, err
	}
	if res.Exit != 0 {
		return nil, fmt.Errorf("plutil cannot read %s: %s", plist, lastLines(res.Stderr, 2))
	}
	var doc map[string]any
	if err := json.Unmarshal(res.Stdout, &doc); err != nil {
		return nil, fmt.Errorf("plutil json of %s: %w", plist, err)
	}
	return doc, nil
}

func iosAppInfo(ctx context.Context, r Runner, app string) (iosInfo, error) {
	doc, err := plistJSON(ctx, r, filepath.Join(app, "Info.plist"))
	if err != nil {
		return iosInfo{}, err
	}
	str := func(k string) string { s, _ := doc[k].(string); return s }
	return iosInfo{
		BundleID:    str("CFBundleIdentifier"),
		Version:     str("CFBundleShortVersionString"),
		BuildNumber: str("CFBundleVersion"),
		XcodeBuild:  str("DTXcodeBuild"),
	}, nil
}

var teamIdentifier = regexp.MustCompile(`(?m)^TeamIdentifier=(\S+)`)

// iosSigningOf reads an .app's team and the SHA-1 of its signing
// certificate (the identity hash `security find-identity` prints).
func iosSigningOf(ctx context.Context, r Runner, app, tmpParent string) (Signing, error) {
	res, err := run(ctx, r, Cmd{Argv: []string{"codesign", "-dv", "--verbose=2", app}})
	if err != nil {
		return Signing{}, err
	}
	out := append(append([]byte{}, res.Stderr...), res.Stdout...)
	m := teamIdentifier.FindSubmatch(out)
	if res.Exit != 0 || m == nil {
		return Signing{}, diag(DiagSigningIdentityMissing, app+" carries no code signature with a team: "+lastLines(out, 2),
			"rebuild it signed (perflab build native), or import a signed .app")
	}
	if err := os.MkdirAll(tmpParent, 0o755); err != nil {
		return Signing{}, err
	}
	dir, err := os.MkdirTemp(tmpParent, ".certs-")
	if err != nil {
		return Signing{}, err
	}
	defer os.RemoveAll(dir)
	prefix := filepath.Join(dir, "cert")
	res, err = run(ctx, r, Cmd{Argv: []string{"codesign", "-d", "--extract-certificates=" + prefix, app}})
	if err != nil {
		return Signing{}, err
	}
	der, err := os.ReadFile(prefix + "0")
	if res.Exit != 0 || err != nil {
		return Signing{}, diag(DiagSigningIdentityMissing, "codesign could not extract the signing certificate of "+app,
			"codesign -dvv "+app)
	}
	sum := sha1.Sum(der)
	return Signing{Team: string(m[1]), IdentitySHA1: strings.ToUpper(hex.EncodeToString(sum[:]))}, nil
}

// apkFacts is what aapt2 and apksigner say about an APK.
type apkFacts struct {
	Package     string
	VersionCode string
	VersionName string
	CertSHA256  string
}

var (
	badgingPackage = regexp.MustCompile(`^package: name='([^']+)' versionCode='([^']*)' versionName='([^']*)'`)
	signerSHA256   = regexp.MustCompile(`Signer #1 certificate SHA-256 digest: ([0-9a-f]{64})`)
)

func apkInfo(ctx context.Context, r Runner, apk string) (apkFacts, error) {
	aapt2, err := androidBuildTool("aapt2")
	if err != nil {
		return apkFacts{}, err
	}
	res, err := run(ctx, r, Cmd{Argv: []string{aapt2, "dump", "badging", apk}})
	if err != nil {
		return apkFacts{}, err
	}
	var f apkFacts
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		if m := badgingPackage.FindStringSubmatch(line); m != nil {
			f.Package, f.VersionCode, f.VersionName = m[1], m[2], m[3]
			break
		}
	}
	if f.Package == "" {
		return apkFacts{}, fmt.Errorf("aapt2 dump badging %s printed no package line: %s", apk, lastLines(res.Stderr, 2))
	}
	apksigner, err := androidBuildTool("apksigner")
	if err != nil {
		return apkFacts{}, err
	}
	res, err = run(ctx, r, Cmd{Argv: []string{apksigner, "verify", "--print-certs", apk}})
	if err != nil {
		return apkFacts{}, err
	}
	m := signerSHA256.FindSubmatch(res.Stdout)
	if res.Exit != 0 || m == nil {
		return apkFacts{}, diag(DiagVerifyFailed, apk+" does not verify: "+lastLines(append(res.Stdout, res.Stderr...), 2),
			"rebuild it (perflab build native)")
	}
	f.CertSHA256 = string(m[1])
	return f, nil
}

// androidSDK is ANDROID_HOME, ANDROID_SDK_ROOT or the platform default.
func androidSDK() string {
	for _, v := range []string{"ANDROID_HOME", "ANDROID_SDK_ROOT"} {
		if p := os.Getenv(v); p != "" {
			return p
		}
	}
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Android", "sdk")
	}
	return filepath.Join(home, "Android", "Sdk")
}

// androidBuildTool resolves aapt2, zipalign or apksigner in the newest
// build-tools that has it.
func androidBuildTool(name string) (string, error) {
	dir := filepath.Join(androidSDK(), "build-tools")
	entries, _ := os.ReadDir(dir)
	var versions []string
	for _, e := range entries {
		if e.IsDir() {
			versions = append(versions, e.Name())
		}
	}
	sort.Slice(versions, func(i, j int) bool { return versionLess(versions[j], versions[i]) })
	for _, v := range versions {
		for _, n := range []string{name, name + ".exe", name + ".bat"} {
			if p := filepath.Join(dir, v, n); fileExists(p) {
				return p, nil
			}
		}
	}
	return "", diag(DiagToolMissing, name+" is not in any Android build-tools under "+dir, toolInstall[name])
}

// versionLess orders dotted numeric versions (35.0.0 < 36.0.0 < 36.0.10).
func versionLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		na, ea := strconv.Atoi(pa[i])
		nb, eb := strconv.Atoi(pb[i])
		if ea != nil || eb != nil {
			if pa[i] != pb[i] {
				return pa[i] < pb[i]
			}
			continue
		}
		if na != nb {
			return na < nb
		}
	}
	return len(pa) < len(pb)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, err := in.Stat()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, st.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// copyTree copies src's contents into dst (merging), preserving modes.
func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target)
	})
}

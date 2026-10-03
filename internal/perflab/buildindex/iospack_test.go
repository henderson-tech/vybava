package buildindex

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

var iosTarget = Target{Platform: "ios", Profile: "perf", Kind: KindShell}

// devCert is a self-signed stand-in for an Apple Development certificate
// of team OU; its SHA-1 is what find-identity would print.
func devCert(t *testing.T, ou string) ([]byte, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Apple Development: Test Dev (AAAAAAAAAA)", OrganizationalUnit: []string{ou}},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha1.Sum(der)
	return der, strings.ToUpper(hex.EncodeToString(sum[:]))
}

// scriptIOSTools answers ditto, plutil, codesign and security like macOS
// does for an .app signed with der.
func scriptIOSTools(t *testing.T, f *fakeRunner, der []byte, sha string, team string) *fakeRunner {
	plists := map[string]map[string]any{}
	return f.
		onDo("ditto ", func(c Cmd) Result {
			if err := copyTree(c.Argv[1], c.Argv[2]); err != nil {
				return Result{Exit: 1, Stderr: []byte(err.Error())}
			}
			return ok("")
		}).
		on("codesign -d --entitlements - --xml", ok(`<?xml version="1.0"?><plist version="1.0"><dict><key>application-identifier</key><string>`+team+`.app.fixit.client</string></dict></plist>`)).
		onDo("plutil -replace", func(c Cmd) Result {
			file := c.Argv[len(c.Argv)-1]
			if plists[file] == nil {
				plists[file] = map[string]any{}
			}
			plists[file][c.Argv[2]] = c.Argv[4]
			return ok("")
		}).
		onDo("plutil -convert json", func(c Cmd) Result {
			file := c.Argv[len(c.Argv)-1]
			doc := map[string]any{"CFBundleIdentifier": "app.fixit.client", "CFBundleShortVersionString": "2.6.0", "CFBundleVersion": "1", "DTXcodeBuild": "17F42"}
			for k, v := range plists[file] {
				doc[k] = v
			}
			b, _ := json.Marshal(doc)
			return ok(string(b))
		}).
		on("codesign -dv --verbose=2", Result{Stderr: []byte("Executable=/x/FixIt.app/FixIt\nIdentifier=app.fixit.client\nTeamIdentifier=" + team + "\n")}).
		onDo("--extract-certificates=", func(c Cmd) Result {
			prefix := strings.TrimPrefix(c.Argv[2], "--extract-certificates=")
			_ = os.WriteFile(prefix+"0", der, 0o644)
			return ok("")
		}).
		on("security find-identity -v -p codesigning", ok("  1) 0A421AE6657EB34EFB9848C7F273F4940DE31F75 \"Developer ID Application: Test Dev (BBBBBBBBBB)\"\n  2) "+sha+" \"Apple Development: Test Dev (AAAAAAAAAA)\"\n     2 valid identities found\n")).
		on("security find-certificate -a -p", ok(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))).
		on("codesign --force --sign "+sha, ok("")).
		on("codesign --verify --deep --strict", ok(""))
}

func TestPackIOSSwapsResealsAndStamps(t *testing.T) {
	s := testStore(t)
	der, sha := devCert(t, "YJ77YV2PNA")
	st, err := newStage(s.buildDir(iosTarget, "pf1-aabbccddeeff00112233"))
	if err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(st.Dir, "FixIt.app")
	for name, body := range map[string]string{"FixIt": "macho", "Info.plist": "plist", "Expo.plist": "plist", "_CodeSignature/CodeResources": "seal"} {
		_ = os.MkdirAll(filepath.Dir(filepath.Join(app, name)), 0o755)
		_ = os.WriteFile(filepath.Join(app, name), []byte(body), 0o644)
	}
	native, err := s.commitBuild(st, BuildManifest{Key: "pf1-aabbccddeeff00112233", SourceKey: "pf1s-src", Platform: "ios", Profile: "perf", Kind: KindShell,
		AppID: "app.fixit.client", Artifact: "FixIt.app", CreatedAt: time.Now().UTC(), EnvNames: []string{}}, Fingerprint{Lines: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	// a stored iOS bundle: main.jsbundle + assets-dest mirroring the app root
	bundleSHA := sha256Hex(hbcBundle)
	bdir := filepath.Join(s.Dirs.bundlesDir(), bundleSHA[:16])
	_ = os.MkdirAll(filepath.Join(bdir, "assets", "assets", "assets", "images"), 0o755)
	_ = os.WriteFile(filepath.Join(bdir, "main.jsbundle"), hbcBundle, 0o644)
	_ = os.WriteFile(filepath.Join(bdir, "assets", "assets", "assets", "images", "icon.png"), []byte("png"), 0o644)
	if err := writeJSONAtomic(filepath.Join(bdir, manifestFile), BundleManifest{SHA256: bundleSHA, Platform: "ios", Profile: "perf", SourceKey: "pf1s-src", File: "main.jsbundle", HBC: true, CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}

	r := scriptIOSTools(t, newFake(t), der, sha, "YJ77YV2PNA")
	res, err := s.Pack(context.Background(), r, PackSpec{NativeKey: native.Build.Manifest.Key, BundleSHA: bundleSHA})
	if err != nil {
		t.Fatal(err)
	}
	v := res.Variant
	if !fileExists(filepath.Join(v.Path, "main.jsbundle")) || !fileExists(filepath.Join(v.Path, "assets", "assets", "images", "icon.png")) {
		t.Fatal("bundle or assets not placed in the app root")
	}
	if !regexp.MustCompile(`^1\.\d+$`).MatchString(v.Manifest.BundleVersion) || v.Manifest.OriginalBundleVersion != "1" || v.Manifest.IdentitySHA1 != sha || !v.Manifest.ExpoUpdatesDisabled {
		t.Fatalf("variant = %+v", v.Manifest)
	}
	if !r.called("plutil -replace EXUpdatesEnabled -bool NO") || !r.called("plutil -replace CFBundleVersion -string "+v.Manifest.BundleVersion) {
		t.Fatalf("expo-updates off and the stamp must be written: %v", r.calls)
	}
	if !r.called("--entitlements") || !r.called("--generate-entitlement-der") || !r.called("codesign --verify --deep --strict") {
		t.Fatalf("re-sign and verify missing: %v", r.calls)
	}
	if fileExists(filepath.Join(native.Build.Path, "main.jsbundle")) {
		t.Fatal("pack wrote into the indexed native build")
	}
}

func TestPackIOSRefusesAnIdentityTheKeychainLacks(t *testing.T) {
	s := testStore(t)
	der, _ := devCert(t, "YJ77YV2PNA")
	st, _ := newStage(s.buildDir(iosTarget, "pf1-aabbccddeeff00112244"))
	_ = os.MkdirAll(filepath.Join(st.Dir, "FixIt.app"), 0o755)
	_ = os.WriteFile(filepath.Join(st.Dir, "FixIt.app", "Info.plist"), []byte("plist"), 0o644)
	native, err := s.commitBuild(st, BuildManifest{Key: "pf1-aabbccddeeff00112244", SourceKey: "pf1s-src", Platform: "ios", Profile: "perf", Kind: KindShell,
		AppID: "app.fixit.client", Artifact: "FixIt.app", CreatedAt: time.Now().UTC(), EnvNames: []string{}}, Fingerprint{Lines: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	bundleSHA := sha256Hex(hbcBundle)
	bdir := filepath.Join(s.Dirs.bundlesDir(), bundleSHA[:16])
	_ = os.MkdirAll(bdir, 0o755)
	_ = os.WriteFile(filepath.Join(bdir, "main.jsbundle"), hbcBundle, 0o644)
	_ = writeJSONAtomic(filepath.Join(bdir, manifestFile), BundleManifest{SHA256: bundleSHA, Platform: "ios", Profile: "perf", SourceKey: "pf1s-src", File: "main.jsbundle", CreatedAt: time.Now().UTC()})
	r := scriptIOSTools(t, newFake(t), der, "0000000000000000000000000000000000000000", "YJ77YV2PNA")
	_, err = s.Pack(context.Background(), r, PackSpec{NativeKey: native.Build.Manifest.Key, BundleSHA: bundleSHA})
	wantCode(t, err, DiagSigningIdentityMissing)
}

func TestResolveSigningPicksTheTeamsDevelopmentIdentity(t *testing.T) {
	der, sha := devCert(t, "YJ77YV2PNA")
	p := Project{IOS: IOSApp{Team: "YJ77YV2PNA"}}
	got, err := ResolveSigning(context.Background(), scriptIOSTools(t, newFake(t), der, sha, "YJ77YV2PNA"), p, "ios")
	if err != nil {
		t.Fatal(err)
	}
	if got.Team != "YJ77YV2PNA" || got.IdentitySHA1 != sha {
		t.Fatalf("signing = %+v", got)
	}
	p.IOS.Team = "ZZZZZZZZZZ"
	_, err = ResolveSigning(context.Background(), scriptIOSTools(t, newFake(t), der, sha, "YJ77YV2PNA"), p, "ios")
	wantCode(t, err, DiagSigningIdentityMissing)
	android, err := ResolveSigning(context.Background(), newFake(t), Project{RepoRoot: t.TempDir(), AppRoot: "apps/client"}, "android")
	if err != nil || android.KeystoreCertSHA256 != TemplateDebugKeystoreSHA256 {
		t.Fatalf("an unprebuilt android app signs with the template key: %+v %v", android, err)
	}
}

func TestBuildNativeIOSShellRecipe(t *testing.T) {
	s := testStore(t)
	der, sha := devCert(t, "YJ77YV2PNA")
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "bun.lock"), []byte("{}"), 0o644)
	p := Project{RepoRoot: root, AppRoot: "apps/client", IOS: IOSApp{Scheme: "FixIt", BundleID: "app.fixit.client", Team: "YJ77YV2PNA"}}
	fp := Fingerprint{Key: "pf1-1234123412341234aaaa", SourceKey: "pf1s-src", Signing: Signing{Team: "YJ77YV2PNA", IdentitySHA1: sha}, Lines: []string{"x"}}
	var xcodeEnv []string
	r := scriptIOSTools(t, newFake(t), der, sha, "YJ77YV2PNA").
		on("bunx expo prebuild --platform ios", ok("✔ Finished prebuild\n")).
		onDo("xcodebuild -workspace", func(c Cmd) Result {
			xcodeEnv = c.Env
			var derived string
			for i, a := range c.Argv {
				if a == "-derivedDataPath" {
					derived = c.Argv[i+1]
				}
			}
			app := filepath.Join(derived, "Build", "Products", "Release-iphoneos", "FixIt.app")
			_ = os.MkdirAll(app, 0o755)
			_ = os.WriteFile(filepath.Join(app, "Info.plist"), []byte("plist"), 0o644)
			return ok("** BUILD SUCCEEDED **\n")
		}).
		on("rev-parse HEAD", ok("3e9268eb7e\n")).
		on("status --porcelain", ok(""))
	env, _ := ParseProfileEnv([]byte("EXPO_PUBLIC_PERF_MODE=1\n"))
	res, err := s.BuildNative(context.Background(), r, BuildSpec{Project: p, Target: iosTarget, Env: env, Fingerprint: &fp})
	if err != nil {
		t.Fatal(err)
	}
	if res.Build.Manifest.AppID != "app.fixit.client" || res.Build.Manifest.BuildNumber != "1" || !fileExists(res.Build.Path) {
		t.Fatalf("build = %+v", res.Build)
	}
	for _, want := range []string{"SKIP_BUNDLING=1", "DEVELOPMENT_TEAM=YJ77YV2PNA", "-allowProvisioningUpdates", "-destination generic/platform=iOS"} {
		if !r.called(want) {
			t.Errorf("xcodebuild lacks %s", want)
		}
	}
	joined := strings.Join(xcodeEnv, " ")
	if !strings.Contains(joined, "EXPO_NO_DOTENV=1") || !strings.Contains(joined, "EXPO_PUBLIC_PERF_MODE=1") {
		t.Fatalf("xcodebuild env lacks the profile: %v", xcodeEnv)
	}
	if res.Next[0] != "perflab bundle export --platform ios --profile perf --json" {
		t.Fatalf("next = %v", res.Next)
	}
}

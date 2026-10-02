package buildindex

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

var hbcBundle = append(append([]byte{}, hbcMagic...), 0x62, 0, 0, 0, 'h', 'b', 'c')

// fakeAndroidSDK points ANDROID_HOME at build-tools stubs; the fake runner
// answers their commands.
func fakeAndroidSDK(t *testing.T) string {
	t.Helper()
	sdk := t.TempDir()
	bt := filepath.Join(sdk, "build-tools", "36.0.0")
	if err := os.MkdirAll(bt, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"aapt2", "zipalign", "apksigner"} {
		if err := os.WriteFile(filepath.Join(bt, tool), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("ANDROID_HOME", sdk)
	return sdk
}

type zipEntry struct {
	name   string
	body   []byte
	stored bool
}

func writeZip(t *testing.T, path string, entries []zipEntry) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		method := zip.Deflate
		if e.stored {
			method = zip.Store
		}
		w, err := zw.CreateHeader(&zip.FileHeader{Name: e.name, Method: method})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(e.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readZip(t *testing.T, path string) map[string]*zip.File {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { zr.Close() })
	out := map[string]*zip.File{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadAll(rc); err != nil { // checks the CRC
			t.Fatalf("%s: %v", f.Name, err)
		}
		rc.Close()
		out[f.Name] = f
	}
	return out
}

func zipBody(t *testing.T, f *zip.File) []byte {
	t.Helper()
	rc, err := f.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	return b
}

var jpgBytes = []byte("\xff\xd8\xff\xe0 aurora poster")

// nativeAPK is a Release APK shaped like the S20 perf build: a stored HBC
// bundle and resources.arsc, a JPG under an obfuscated res path, v1
// signature files.
func nativeAPK(t *testing.T, path string) {
	writeZip(t, path, []zipEntry{
		{name: "AndroidManifest.xml", body: encodeAXML(t, []metaData{{name: expoUpdatesEnabledKey, value: true}, {name: "expo.modules.updates.EXPO_UPDATES_CHECK_ON_LAUNCH", value: "ALWAYS"}})},
		{name: "classes.dex", body: bytes.Repeat([]byte("dex"), 100)},
		{name: "resources.arsc", body: []byte("arsc table"), stored: true},
		{name: "lib/arm64-v8a/libhermesvm.so", body: []byte("\x7fELF hermes"), stored: true},
		{name: "assets/app.manifest", body: []byte(`{"id":"embedded"}`)},
		{name: "assets/index.android.bundle", body: append(append([]byte{}, hbcMagic...), 0x62, 0, 0, 0, 'o', 'l', 'd'), stored: true},
		{name: "res/bM.jpg", body: jpgBytes},
		{name: "res/VQ.png", body: []byte("crunched png")},
		{name: "META-INF/MANIFEST.MF", body: []byte("Manifest-Version: 1.0")},
		{name: "META-INF/CERT.SF", body: []byte("sf")},
		{name: "META-INF/CERT.RSA", body: []byte("rsa")},
		{name: "META-INF/androidx.activity_activity.version", body: []byte("1.9.0")},
	})
}

func androidProject(t *testing.T) Project {
	t.Helper()
	root := t.TempDir()
	app := filepath.Join(root, "apps", "client")
	if err := os.MkdirAll(filepath.Join(app, "android", "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app, "android", "app", "debug.keystore"), []byte("template keystore"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bun.lock"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	return Project{ID: "henderson-tech/FixIt", RepoRoot: root, AppRoot: "apps/client", Entry: "index.js",
		Android: AndroidApp{Package: "app.fixit.client.dev"}}
}

// scriptAndroidTools answers aapt2, apksigner, zipalign and git like the
// real tools would for nativeAPK.
func scriptAndroidTools(f *fakeRunner) *fakeRunner {
	resources, _ := os.ReadFile(filepath.Join("testdata", "aapt2-dump-resources.txt"))
	return f.
		on("aapt2 dump badging", ok("package: name='app.fixit.client.dev' versionCode='1' versionName='2.6.0' platformBuildVersionName='16'\nminSdkVersion:'24'\n")).
		on("aapt2 dump resources", ok(string(resources))).
		on("apksigner verify --print-certs", ok("Signer #1 certificate DN: CN=Android Debug, OU=Android, O=Unknown, L=Unknown, ST=Unknown, C=US\nSigner #1 certificate SHA-256 digest: "+TemplateDebugKeystoreSHA256+"\n")).
		onDo("zipalign -p -f 4", func(c Cmd) Result {
			n := len(c.Argv)
			if err := copyFile(c.Argv[n-2], c.Argv[n-1]); err != nil {
				return Result{Exit: 1, Stderr: []byte(err.Error())}
			}
			return ok("")
		}).
		onDo("apksigner sign", func(c Cmd) Result {
			var out string
			for i, a := range c.Argv {
				if a == "--out" {
					out = c.Argv[i+1]
				}
			}
			if err := copyFile(c.Argv[len(c.Argv)-1], out); err != nil {
				return Result{Exit: 1, Stderr: []byte(err.Error())}
			}
			return ok("")
		}).
		on("rev-parse HEAD", ok("3e9268eb7e0000000000000000000000000000ab\n")).
		on("status --porcelain", ok(""))
}

func TestBuildNativeAndroidThroughTheAdapterCommand(t *testing.T) {
	fakeAndroidSDK(t)
	s := testStore(t)
	p := androidProject(t)
	p.AndroidBuildCmd = []string{"bun", "scripts/perf/build-android.ts", "--out", "{outDir}"}
	fp := Fingerprint{Key: "pf1-ffffffffffffffffffff", SourceKey: "pf1s-src", Signing: Signing{KeystoreCertSHA256: TemplateDebugKeystoreSHA256}, Lines: []string{"x"}}
	var outDir string
	r := scriptAndroidTools(newFake(t)).onDo("build-android.ts", func(c Cmd) Result {
		outDir = c.Argv[len(c.Argv)-1]
		nativeAPK(t, filepath.Join(outDir, "app-release.apk"))
		_ = os.WriteFile(filepath.Join(outDir, "fixit-build-metadata.json"), []byte(`{"instrumentation":{"productionEquivalent":true},"publicEnvHash":"abc"}`), 0o644)
		return ok("BUILD SUCCESSFUL\n")
	})
	env, _ := ParseProfileEnv([]byte("EXPO_PUBLIC_PERF_MODE=1\n"))
	res, err := s.BuildNative(context.Background(), r, BuildSpec{Project: p, Target: androidTarget, Env: env, Fingerprint: &fp})
	if err != nil {
		t.Fatal(err)
	}
	m := res.Build.Manifest
	if res.Hit || m.AppID != "app.fixit.client.dev" || m.Version != "2.6.0" || m.BuildNumber != "1" || m.ArtifactSHA256 == "" {
		t.Fatalf("manifest = %+v", m)
	}
	if !m.ProductionEquivalent || m.PublicEnvHash != "abc" || m.Keystore != "debug.keystore" || m.Source.Commit == "" || m.Source.Dirty {
		t.Fatalf("provenance = %+v", m)
	}
	if !strings.Contains(outDir, ".tmp-") {
		t.Fatalf("{outDir} not filled with the staging dir: %q", outDir)
	}
	if !fileExists(filepath.Join(res.Build.Dir, "build.log")) || !fileExists(res.Build.Path) {
		t.Fatal("log or artifact missing from the entry")
	}
	again, err := s.BuildNative(context.Background(), newFake(t), BuildSpec{Project: p, Target: androidTarget, Fingerprint: &fp})
	if err != nil || !again.Hit {
		t.Fatalf("the rebuild should be a hit: %v %+v", err, again)
	}
}

func TestBuildNativeFailureKeepsTheLog(t *testing.T) {
	fakeAndroidSDK(t)
	s := testStore(t)
	p := androidProject(t)
	p.AndroidBuildCmd = []string{"bun", "scripts/perf/build-android.ts", "{outDir}"}
	fp := Fingerprint{Key: "pf1-99999999999999999999"}
	r := newFake(t).on("build-android.ts", Result{Exit: 1, Stdout: []byte("FAILURE: Build failed with an exception.\n")})
	_, err := s.BuildNative(context.Background(), r, BuildSpec{Project: p, Target: androidTarget, Fingerprint: &fp})
	d := wantCode(t, err, DiagBuildFailed)
	logs, _ := filepath.Glob(filepath.Join(s.Dirs.Cache, "logs", "build-pf1-99999999999999999999-*.log"))
	if len(logs) != 1 || !strings.Contains(d.Diag.Detail, logs[0]) {
		t.Fatalf("the failed log is not kept and named: %v / %s", logs, d.Diag.Detail)
	}
	if _, err := s.FindBuild(androidTarget, fp.Key); err == nil {
		t.Fatal("a failed build left an entry")
	}
}

// exportFake scripts a fingerprint run (the real excerpt), an export:embed
// writing plain JS plus assets, react-native's resolution and a hermesc
// that compiles the JS into body.
func exportFake(t *testing.T, body []byte, assets map[string][]byte) *fakeRunner {
	raw := loadExcerpt(t)
	rn := t.TempDir()
	hermesc := filepath.Join(rn, "sdks", "hermesc", map[string]string{"darwin": "osx-bin/hermesc", "linux": "linux64-bin/hermesc", "windows": "win64-bin/hermesc.exe"}[runtime.GOOS])
	_ = os.MkdirAll(filepath.Dir(hermesc), 0o755)
	_ = os.WriteFile(hermesc, []byte("#!/bin/sh\n"), 0o755)
	return scriptAndroidTools(newFake(t)).
		on("fingerprint:generate", ok(string(raw))).
		onDo("export:embed", func(c Cmd) Result {
			var out, dest string
			for i, a := range c.Argv {
				switch a {
				case "--bundle-output":
					out = c.Argv[i+1]
				case "--assets-dest":
					dest = c.Argv[i+1]
				}
			}
			if filepath.Base(out) != hermesInput || !strings.Contains(strings.Join(c.Argv, " "), "--minify false --bytecode false") {
				t.Errorf("export must write unminified JS for hermesc: %v", c.Argv)
			}
			if err := os.WriteFile(out, []byte("var __BUNDLE_START_TIME__=1;"), 0o644); err != nil {
				return Result{Exit: 1}
			}
			for rel, b := range assets {
				_ = os.MkdirAll(filepath.Join(dest, filepath.Dir(rel)), 0o755)
				_ = os.WriteFile(filepath.Join(dest, rel), b, 0o644)
			}
			return ok("Android Bundled 812ms index.js (10594 modules)\nWriting bundle output to: " + out + "\n")
		}).
		on("node -e", ok(rn+"\n\n")).
		onDo(hermesc, func(c Cmd) Result {
			want := []string{hermesc, "-emit-binary", "-out", c.Argv[3], hermesInput, "-O"}
			if strings.Join(c.Argv, " ") != strings.Join(want, " ") || !fileExists(filepath.Join(c.Dir, hermesInput)) {
				t.Errorf("hermesc must compile the fixed relative %s from the stage: %v in %s", hermesInput, c.Argv, c.Dir)
			}
			if err := os.WriteFile(c.Argv[3], body, 0o644); err != nil {
				return Result{Exit: 1}
			}
			return ok("")
		})
}

func TestExportBundleStoresHermesBytecodeOnce(t *testing.T) {
	fakeAndroidSDK(t)
	s := testStore(t)
	p := androidProject(t)
	assets := map[string][]byte{"drawable-mdpi/assets_gradients_auroradarkposter.jpg": jpgBytes}
	spec := BundleSpec{Project: p, Platform: "android", Profile: "perf", Label: "before"}
	first, err := s.ExportBundle(context.Background(), exportFake(t, hbcBundle, assets), spec)
	if err != nil {
		t.Fatal(err)
	}
	m := first.Bundle.Manifest
	if first.Hit || !m.HBC || m.HBCVersion != 0x62 || m.Modules != 10594 || m.AssetsCount != 1 || m.File != "index.android.bundle" {
		t.Fatalf("manifest = %+v", m)
	}
	if filepath.Base(first.Bundle.Dir) != m.SHA256[:16] || !strings.HasPrefix(m.SourceKey, "pf1s-") {
		t.Fatalf("not content addressed: %s %+v", first.Bundle.Dir, m)
	}
	second, err := s.ExportBundle(context.Background(), exportFake(t, hbcBundle, assets), spec)
	if err != nil || !second.Hit || second.Bundle.Dir != first.Bundle.Dir {
		t.Fatalf("a re-export of the same bytes must answer the stored bundle: %v %+v", err, second)
	}
	_, err = s.ExportBundle(context.Background(), exportFake(t, []byte("var __BUNDLE_START_TIME__=1;"), nil), spec) // a hermesc that emits JS
	wantCode(t, err, DiagBundleNotHBC)
}

// packFixture stores a native APK whose source key matches the excerpt's
// android export, and exports a bundle with assets.
func packFixture(t *testing.T, assets map[string][]byte, recorded map[string]string) (Store, Build, Bundle) {
	t.Helper()
	fakeAndroidSDK(t)
	s := testStore(t)
	p := androidProject(t)
	src, err := ComputeKey(loadExcerpt(t), KeyInputs{Platform: "android", Profile: "perf"})
	if err != nil {
		t.Fatal(err)
	}
	st, err := newStage(s.buildDir(androidTarget, "pf1-0123456789abcdef0123"))
	if err != nil {
		t.Fatal(err)
	}
	nativeAPK(t, filepath.Join(st.Dir, "app-release.apk"))
	_ = os.WriteFile(filepath.Join(st.Dir, "debug.keystore"), []byte("template keystore"), 0o644)
	built, err := s.commitBuild(st, BuildManifest{Key: "pf1-0123456789abcdef0123", SourceKey: src.SourceKey, Platform: "android", Profile: "perf",
		Kind: KindBundled, AppID: "app.fixit.client.dev", Artifact: "app-release.apk", Keystore: "debug.keystore", AndroidAssets: recorded,
		Signing: Signing{KeystoreCertSHA256: TemplateDebugKeystoreSHA256}, CreatedAt: time.Now().UTC(), EnvNames: []string{}}, Fingerprint{Lines: []string{"x"}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.ExportBundle(context.Background(), exportFake(t, hbcBundle, assets), BundleSpec{Project: p, Platform: "android", Profile: "perf"})
	if err != nil {
		t.Fatal(err)
	}
	return s, built.Build, b.Bundle
}

func TestPackAndroidSwapsTheBundleAndResigns(t *testing.T) {
	s, native, bundle := packFixture(t, map[string][]byte{
		"drawable-mdpi/assets_gradients_auroradarkposter.jpg": jpgBytes,
		"drawable-xhdpi/assets_images_headerplus.png":         []byte("source png, crunched in the APK"),
	}, nil)
	res, err := s.Pack(context.Background(), scriptAndroidTools(newFake(t)), PackSpec{NativeKey: native.Manifest.Key, BundleSHA: bundle.Manifest.SHA256[:16]})
	if err != nil {
		t.Fatal(err)
	}
	v := res.Variant.Manifest
	if v.VariantID != "pf1-01234567-"+bundle.Manifest.SHA256[:12] || v.ProductionEquivalent || v.ArtifactSHA256 == "" || v.AppID != "app.fixit.client.dev" {
		t.Fatalf("variant = %+v", v)
	}
	if len(res.Diagnostics) != 2 || res.Diagnostics[0].Code != DiagNotProductionEquivalent ||
		res.Diagnostics[1].Code != DiagBundleAssetsChanged || res.Diagnostics[1].Severity != "info" {
		t.Fatalf("an imported APK reports its compiled assets as unverified: %+v", res.Diagnostics)
	}
	entries := readZip(t, res.Variant.Path)
	if got := zipBody(t, entries["assets/index.android.bundle"]); !bytes.Equal(got, hbcBundle) {
		t.Fatalf("bundle not swapped: %q", got)
	}
	if !v.ExpoUpdatesDisabled || expoUpdatesValue(t, zipBody(t, entries["AndroidManifest.xml"])) != 0 {
		t.Fatal("expo-updates must be off in the variant, or an OTA update could replace the swapped bundle")
	}
	for _, name := range []string{"assets/index.android.bundle", "resources.arsc", "lib/arm64-v8a/libhermesvm.so"} {
		if entries[name].Method != zip.Store {
			t.Errorf("%s lost its stored layout", name)
		}
	}
	for _, gone := range []string{"META-INF/MANIFEST.MF", "META-INF/CERT.SF", "META-INF/CERT.RSA"} {
		if entries[gone] != nil {
			t.Errorf("%s kept: the old v1 signature must be dropped", gone)
		}
	}
	if entries["META-INF/androidx.activity_activity.version"] == nil || entries["classes.dex"] == nil || entries["assets/app.manifest"] == nil {
		t.Fatal("unrelated entries were dropped")
	}
	again, err := s.Pack(context.Background(), newFake(t), PackSpec{NativeKey: native.Manifest.Key, BundleSHA: bundle.Manifest.SHA256})
	if err != nil || !again.Hit {
		t.Fatalf("packing twice must answer the stored variant: %v %+v", err, again)
	}
}

func TestPackAndroidRefusesAssetsTheAPKLacks(t *testing.T) {
	png := []byte("source png")
	builtWith := map[string]string{"drawable-xhdpi/assets_images_headerplus.png": sha256Hex(png)}
	cases := map[string]struct {
		assets   map[string][]byte
		recorded map[string]string
	}{
		"a new image": {assets: map[string][]byte{
			"drawable-mdpi/assets_gradients_auroradarkposter.jpg": jpgBytes,
			"drawable-mdpi/assets_images_brandnew.png":            []byte("png"),
		}},
		"a new density":  {assets: map[string][]byte{"drawable-xxxhdpi/assets_images_headerplus.png": []byte("png")}},
		"changed pixels": {assets: map[string][]byte{"drawable-mdpi/assets_gradients_auroradarkposter.jpg": []byte("\xff\xd8\xff\xe0 a new poster")}},
		"a redrawn png the engine build recorded": {assets: map[string][]byte{"drawable-xhdpi/assets_images_headerplus.png": []byte("redrawn png")}, recorded: builtWith},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s, native, bundle := packFixture(t, tc.assets, tc.recorded)
			_, err := s.Pack(context.Background(), scriptAndroidTools(newFake(t)), PackSpec{NativeKey: native.Manifest.Key, BundleSHA: bundle.Manifest.SHA256})
			wantCode(t, err, DiagBundleAssetsChanged)
		})
	}
	t.Run("the same png the engine build recorded", func(t *testing.T) {
		s, native, bundle := packFixture(t, map[string][]byte{"drawable-xhdpi/assets_images_headerplus.png": png}, builtWith)
		res, err := s.Pack(context.Background(), scriptAndroidTools(newFake(t)), PackSpec{NativeKey: native.Manifest.Key, BundleSHA: bundle.Manifest.SHA256})
		if err != nil || len(res.Diagnostics) != 1 {
			t.Fatalf("a verified swap carries only NOT_PRODUCTION_EQUIVALENT: %v %+v", err, res.Diagnostics)
		}
	})
}

func TestPackRefusesAnotherNativeSourceKey(t *testing.T) {
	s, native, bundle := packFixture(t, nil, nil)
	m := bundle.Manifest
	m.SourceKey = "pf1s-from-another-tree"
	if err := writeJSONAtomic(filepath.Join(bundle.Dir, manifestFile), m); err != nil {
		t.Fatal(err)
	}
	_, err := s.Pack(context.Background(), newFake(t), PackSpec{NativeKey: native.Manifest.Key, BundleSHA: m.SHA256})
	d := wantCode(t, err, DiagFingerprintMismatch)
	if !strings.Contains(d.Diag.Fix, "perflab build native --platform android") {
		t.Fatalf("fix = %s", d.Diag.Fix)
	}
}

func TestApkResourcesParsesTheDump(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "aapt2-dump-resources.txt"))
	if err != nil {
		t.Fatal(err)
	}
	res := apkResources(raw)
	want := map[string]string{
		"drawable/assets_gradients_auroradarkposter|mdpi": "res/bM.jpg",
		"drawable/assets_images_headerplus|xhdpi":         "res/yG.png",
		"drawable/assets_images_headerplus|xxhdpi":        "res/MH1.png",
	}
	for k, v := range want {
		if res[k] != v {
			t.Errorf("%s = %q, want %q", k, res[k], v)
		}
	}
	for k := range res {
		if strings.HasPrefix(k, "raw/") && !strings.HasSuffix(k, "|") {
			t.Errorf("a default-config raw resource keyed %q", k)
		}
	}
	for in, want := range map[string]string{
		"drawable-mdpi/assets_images_headerplus.png": "drawable/assets_images_headerplus|mdpi",
		"drawable-hdpi-v4/x.9.png":                   "drawable/x|hdpi",
		"raw/data.json":                              "raw/data|",
	} {
		if got, _ := assetResourceKey(in); got != want {
			t.Errorf("assetResourceKey(%q) = %q, want %q", in, got, want)
		}
	}
}

// An import recomputes the key from THIS checkout, so an artifact whose
// marketing version differs from the app config's (a 2.6.0 APK in a 4.0.0
// checkout) was built from other native inputs and is never filed under it.
func TestImportRefusesAnArtifactOfAnotherAppVersion(t *testing.T) {
	fakeAndroidSDK(t)
	t.Setenv("JAVA_HOME", "/opt/jdk21") // Linux reads it; macOS asks java_home (scripted below)
	s := testStore(t)
	p := androidProject(t)
	apk := filepath.Join(t.TempDir(), "app-release.apk")
	nativeAPK(t, apk) // versionName 2.6.0 (scriptAndroidTools' badging)
	var doc map[string]any
	if err := json.Unmarshal(loadExcerpt(t), &doc); err != nil {
		t.Fatal(err)
	}
	doc["sources"] = append(doc["sources"].([]any), map[string]any{"type": "contents", "id": "expoConfig", "hash": "x",
		"contents": `{"name":"FixIt","slug":"fixit-client","version":"4.0.0","runtimeVersion":{"policy":"appVersion"}}`})
	fpJSON, _ := json.Marshal(doc)
	r := scriptAndroidTools(newFake(t)).
		on("java_home", ok("/opt/jdk21\n")).
		on("java -version", Result{Stderr: []byte(`openjdk version "21.0.10" 2026-01-20` + "\n")}).
		on("fingerprint:generate", ok(string(fpJSON)))
	_, err := s.Import(context.Background(), r, ImportSpec{Project: p, Target: androidTarget, Artifact: apk})
	d := wantCode(t, err, DiagFingerprintMismatch)
	if !strings.Contains(d.Diag.Detail, "version 2.6.0") || !strings.Contains(d.Diag.Detail, "says 4.0.0") ||
		d.Diag.Fix != "perflab build native --platform android --profile perf --kind bundled --json" {
		t.Fatalf("diag = %+v", d.Diag)
	}
	if builds, _ := s.ListBuilds(); len(builds) != 0 {
		t.Fatalf("a refused import left %d index entries", len(builds))
	}
}

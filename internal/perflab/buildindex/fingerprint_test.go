package buildindex

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The excerpt is 11 sources of a real `fingerprint:generate apps/client
// --platform ios --debug` run in a FixIt worktree (.worktrees/<slug>, so
// bun's isolated links sit 8 levels up), contents trimmed.
func loadExcerpt(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "fp-ios-debug-excerpt.json"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// reroot rewrites the excerpt as the main checkout would print it: the
// links 6 levels up, and every dir/contents/top hash different (Expo hashes
// the relative paths).
func reroot(t *testing.T, raw []byte) []byte {
	t.Helper()
	s := strings.ReplaceAll(string(raw), strings.Repeat("../", 8), strings.Repeat("../", 6))
	var doc map[string]any
	if err := json.Unmarshal([]byte(s), &doc); err != nil {
		t.Fatal(err)
	}
	doc["hash"] = "0000000000000000000000000000000000000001"
	for _, src := range doc["sources"].([]any) {
		m := src.(map[string]any)
		if m["type"] != "file" && m["hash"] != nil {
			m["hash"] = "ffffffffffffffffffffffffffffffffffffffff"
			if dbg, ok := m["debugInfo"].(map[string]any); ok {
				dbg["hash"] = "ffffffffffffffffffffffffffffffffffffffff"
			}
		}
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func baseInputs() KeyInputs {
	return KeyInputs{
		Platform: "ios", Profile: "perf", Kind: KindShell,
		Signing:   Signing{Team: "YJ77YV2PNA", IdentitySHA1: "48D7A61DDC76FF4F1FF6CE9902DAA87DACCF14DA"},
		Toolchain: Toolchain{Xcode: "17F42"},
	}
}

func TestComputeKeyIsPortableAcrossCheckoutDepth(t *testing.T) {
	raw := loadExcerpt(t)
	wt, err := ComputeKey(raw, baseInputs())
	if err != nil {
		t.Fatal(err)
	}
	main, err := ComputeKey(reroot(t, raw), baseInputs())
	if err != nil {
		t.Fatal(err)
	}
	if wt.ExpoHash == main.ExpoHash {
		t.Fatal("the re-rooted fixture should carry another Expo hash")
	}
	if wt.Key != main.Key || wt.SourceKey != main.SourceKey {
		t.Fatalf("depth changed the key: %s/%s vs %s/%s", wt.Key, wt.SourceKey, main.Key, main.SourceKey)
	}
	if !strings.HasPrefix(wt.Key, "pf1-") || len(wt.Key) != 24 || !strings.HasPrefix(wt.SourceKey, "pf1s-") {
		t.Fatalf("key shape: %s %s", wt.Key, wt.SourceKey)
	}
	want := SourceCounts{Files: 3, Dirs: 5, Leaves: 8, NullDirs: 2, Contents: 3, Skipped: 1}
	if wt.Sources != want {
		t.Fatalf("counts = %+v, want %+v", wt.Sources, want)
	}
	for _, l := range wt.Lines {
		if strings.Contains(l, "../") {
			t.Fatalf("a key line kept a relative prefix: %q", l)
		}
	}
}

func TestComputeKeyChangesWithEveryNativeInput(t *testing.T) {
	raw := loadExcerpt(t)
	base, err := ComputeKey(raw, baseInputs())
	if err != nil {
		t.Fatal(err)
	}
	edit := func(f func(doc map[string]any)) []byte {
		var doc map[string]any
		_ = json.Unmarshal(raw, &doc)
		f(doc)
		b, _ := json.Marshal(doc)
		return b
	}
	sources := func(doc map[string]any) []any { return doc["sources"].([]any) }
	cases := []struct {
		name          string
		raw           []byte
		in            func(*KeyInputs)
		sourceChanges bool
	}{
		{name: "a leaf file hash", sourceChanges: true, raw: edit(func(d map[string]any) {
			dbg := sources(d)[3].(map[string]any)["debugInfo"].(map[string]any)
			leaf := dbg["children"].([]any)[0].(map[string]any)
			for leaf["children"] != nil {
				leaf = leaf["children"].([]any)[0].(map[string]any)
			}
			leaf["hash"] = "1111111111111111111111111111111111111111"
		})},
		{name: "a symlinked module version (null dir path)", sourceChanges: true, raw: edit(func(d map[string]any) {
			for _, s := range sources(d) {
				m := s.(map[string]any)
				if p, _ := m["filePath"].(string); strings.Contains(p, "expo-camera@57.0.4") {
					m["filePath"] = strings.Replace(p, "57.0.4", "57.0.5", 1)
				}
			}
		})},
		{name: "an autolinking config", sourceChanges: true, raw: edit(func(d map[string]any) {
			for _, s := range sources(d) {
				m := s.(map[string]any)
				if m["id"] == "package:react-native" {
					m["contents"] = `{"name":"react-native","version":"0.86.1"}`
				}
			}
		})},
		{name: "a reanimated static flag via fingerprint.config extraSources", sourceChanges: true, raw: edit(func(d map[string]any) {
			d["sources"] = append(sources(d), map[string]any{"type": "contents", "id": "reanimatedStaticFeatureFlags",
				"contents": `{"ANDROID_SYNCHRONOUSLY_UPDATE_UI_PROPS":false}`, "hash": "x", "reasons": []any{"custom"}})
		})},
		{name: "a reanimated static flag via extraNativeInputs", sourceChanges: true, in: func(k *KeyInputs) {
			k.Extras = []ExtraInput{{Ref: "package.json#/reanimated/staticFeatureFlags", SHA256: "aa"}}
		}},
		{name: "profile", sourceChanges: true, in: func(k *KeyInputs) { k.Profile = "store" }},
		{name: "kind", in: func(k *KeyInputs) { k.Kind = KindBundled }},
		{name: "signing identity", in: func(k *KeyInputs) { k.Signing.IdentitySHA1 = "0A421AE6657EB34EFB9848C7F273F4940DE31F75" }},
		{name: "xcode build", in: func(k *KeyInputs) { k.Toolchain.Xcode = "17E192" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := baseInputs()
			if tc.in != nil {
				tc.in(&in)
			}
			r := raw
			if tc.raw != nil {
				r = tc.raw
			}
			got, err := ComputeKey(r, in)
			if err != nil {
				t.Fatal(err)
			}
			if got.Key == base.Key {
				t.Fatal("the key did not change")
			}
			if (got.SourceKey != base.SourceKey) != tc.sourceChanges {
				t.Fatalf("source key changed=%v, want %v", got.SourceKey != base.SourceKey, tc.sourceChanges)
			}
		})
	}
}

func TestComputeKeyRefusesUnusableOutput(t *testing.T) {
	cases := map[string]string{
		"not json":              "CommandError: ENOENT: no such file or directory, scandir '/.bun/install/cache/links/x'",
		"dir without debugInfo": `{"hash":"a","sources":[{"type":"dir","filePath":"../x","hash":"b","reasons":["rncoreAutolinkingIos"]}]}`,
		"unknown source type":   `{"hash":"a","sources":[{"type":"blob","hash":"b"}]}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ComputeKey([]byte(raw), baseInputs())
			wantCode(t, err, DiagFingerprintFailed)
		})
	}
}

func TestReadExtraInputsHashesThePointedValue(t *testing.T) {
	dir := t.TempDir()
	write := func(flags string) {
		pkg := `{"name":"client","reanimated":{"staticFeatureFlags":` + flags + `}}`
		if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(pkg), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ref := "package.json#/reanimated/staticFeatureFlags"
	read := func() string {
		x, err := ReadExtraInputs(dir, []string{ref})
		if err != nil {
			t.Fatal(err)
		}
		return x[0].SHA256
	}
	write(`{"IOS_SYNCHRONOUSLY_UPDATE_UI_PROPS":true,"ANDROID_SYNCHRONOUSLY_UPDATE_UI_PROPS":true}`)
	on := read()
	write(`{"ANDROID_SYNCHRONOUSLY_UPDATE_UI_PROPS":true,"IOS_SYNCHRONOUSLY_UPDATE_UI_PROPS":true}`)
	if read() != on {
		t.Fatal("key order changed the hash; the JSON is not canonical")
	}
	write(`{"ANDROID_SYNCHRONOUSLY_UPDATE_UI_PROPS":false,"IOS_SYNCHRONOUSLY_UPDATE_UI_PROPS":true}`)
	if read() == on {
		t.Fatal("flipping a flag kept the hash")
	}
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(`{"name":"client"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if read() != sha256Hex([]byte("null")) {
		t.Fatal("a missing pointer must hash as null")
	}
	_, err := ReadExtraInputs(dir, []string{"missing.json#/x"})
	wantCode(t, err, DiagFingerprintFailed)
}

func TestNormalizePath(t *testing.T) {
	roots := []string{"/Users/x/FixIt/.worktrees/a/apps/client", "/Users/x/FixIt/.worktrees/a", "/Users/x"}
	cases := map[string]string{
		"../../../../../../../../.bun/install/cache/links/a/b.js": ".bun/install/cache/links/a/b.js",
		"../../node_modules/.bun/expo@1/node_modules/expo/ios":    "node_modules/.bun/expo@1/node_modules/expo/ios",
		"plugins/with-push-threads.js":                            "plugins/with-push-threads.js",
		"./eas.json":                                              "eas.json",
		"/Users/x/FixIt/.worktrees/a/apps/client/app.json":        "app.json",
		"/Users/x/FixIt/.worktrees/a/node_modules/x":              "node_modules/x",
		"/Users/x/.bun/install/cache/links/a/b.js":                ".bun/install/cache/links/a/b.js",
	}
	for in, want := range cases {
		if got := normalizePath(in, roots); got != want {
			t.Errorf("normalizePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseProfileEnv(t *testing.T) {
	env, err := ParseProfileEnv([]byte("# perf profile\nEXPO_PUBLIC_PERF_MODE=1\nexport EXPO_PUBLIC_API_URL=\"http://localhost:23936\"\n\nAPP_VARIANT=perf\nEXPO_PUBLIC_PERF_MODE=true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(env.Names(), ","); got != "APP_VARIANT,EXPO_PUBLIC_API_URL,EXPO_PUBLIC_PERF_MODE" {
		t.Fatalf("names = %s", got)
	}
	if env.Vars[2] != "EXPO_PUBLIC_PERF_MODE=true" {
		t.Fatalf("a repeated name keeps the last value, got %v", env.Vars)
	}
	reordered, _ := ParseProfileEnv([]byte("APP_VARIANT=perf\nEXPO_PUBLIC_PERF_MODE=true\nEXPO_PUBLIC_API_URL=http://localhost:23936\n"))
	if env.Hash() == "" || env.Hash() != reordered.Hash() {
		t.Fatalf("hash must ignore order: %s vs %s", env.Hash(), reordered.Hash())
	}
	_, err = ParseProfileEnv([]byte("Loading env from .env.local\n"))
	wantCode(t, err, DiagAdapterCommandFailed)
}

package buildindex

import (
	"archive/zip"
	"context"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/runx"
)

// Android pack spike (design 5.5), verdict GO, 2026-10-02 on the Samsung
// S20 (Android 13): the FixIt perf APK (app.fixit.client, template debug
// key) swapped to a bundle whose only change was a prepended logging line
// installed with `adb install -r` (no uninstall), AOT-compiled, passed the
// base-APK sha fence, launched to MainActivity, logged the marker under
// ReactNativeJS and left `logcat -b crash` empty. expo-updates did not
// reject the swapped launch asset (its embedded app.manifest carries no
// launch-asset hash), but its launch check stayed live, hence the ENABLED
// flip below. The incremental-Gradle fallback is therefore not built; a
// bundle whose assets the APK lacks answers BUNDLE_ASSETS_CHANGED with an
// isolated native build of its commit, and the adapter's build.android
// command remains the override for projects whose Gradle build differs.

// packAndroid swaps assets/index.android.bundle inside a copy of the APK,
// refuses a bundle whose assets the APK has no resource for (a swap cannot
// add resources.arsc entries), drops the old v1 signature files, then
// zipaligns and re-signs with the template debug keystore the build was
// signed with. The v2+ signing block is not copied by the rewrite.
func packAndroid(ctx context.Context, r Runner, b Build, bundle Bundle, stage string, vm *VariantManifest, progress *Progress, diags *[]runx.Diagnostic) error {
	nm := b.Manifest
	if nm.Signing.KeystoreCertSHA256 != TemplateDebugKeystoreSHA256 || nm.Keystore == "" {
		return diag(DiagResignFailed,
			"pack re-signs only with React Native's public template debug keystore, and build "+nm.Key+" is signed otherwise (or kept no keystore)",
			nativeFor(bundle.Manifest, KindBundled))
	}
	phase(progress, "assets-check")
	assetDiags, err := checkAndroidAssets(ctx, r, b, bundle)
	if err != nil {
		return err
	}
	*diags = append(*diags, assetDiags...)
	phase(progress, "swap")
	unsigned := filepath.Join(stage, "unsigned.apk")
	updatesOff, err := swapAPKBundle(b.Path, bundle.Path, unsigned)
	if err != nil {
		return err
	}
	vm.ExpoUpdatesDisabled = updatesOff
	zipalign, err := androidBuildTool("zipalign")
	if err != nil {
		return err
	}
	apksigner, err := androidBuildTool("apksigner")
	if err != nil {
		return err
	}
	aligned := filepath.Join(stage, "aligned.apk")
	res, err := run(ctx, r, Cmd{Argv: []string{zipalign, "-p", "-f", "4", unsigned, aligned}, Timeout: 5 * time.Minute})
	if err != nil {
		return err
	}
	if res.Exit != 0 {
		return diag(DiagResignFailed, "zipalign failed: "+lastLines(append(res.Stdout, res.Stderr...), 3), "zipalign -p -f 4 "+unsigned+" "+aligned)
	}
	phase(progress, "resign")
	out := filepath.Join(stage, "app-variant.apk")
	ks := filepath.Join(b.Dir, nm.Keystore)
	res, err = run(ctx, r, Cmd{Argv: []string{apksigner, "sign", "--ks", ks, "--ks-pass", "pass:android",
		"--ks-key-alias", "androiddebugkey", "--key-pass", "pass:android", "--out", out, aligned}, Timeout: 5 * time.Minute})
	if err != nil {
		return err
	}
	if res.Exit != 0 {
		return diag(DiagResignFailed, "apksigner sign failed: "+lastLines(append(res.Stdout, res.Stderr...), 3), "apksigner sign --ks "+ks+" ... "+aligned)
	}
	res, err = run(ctx, r, Cmd{Argv: []string{apksigner, "verify", "--print-certs", out}, Timeout: 5 * time.Minute})
	if err != nil {
		return err
	}
	if m := signerSHA256.FindSubmatch(res.Stdout); res.Exit != 0 || m == nil || string(m[1]) != TemplateDebugKeystoreSHA256 {
		return diag(DiagVerifyFailed, "the re-signed variant does not verify with the template key: "+lastLines(append(res.Stdout, res.Stderr...), 3),
			"apksigner verify --print-certs -v "+out)
	}
	_ = os.Remove(unsigned)
	_ = os.Remove(aligned)
	_ = os.Remove(out + ".idsig")
	sum, err := fileSHA256(out)
	if err != nil {
		return err
	}
	vm.ArtifactSHA256 = sum
	vm.Artifact = filepath.Base(out)
	return nil
}

// swapAPKBundle rewrites src into dst with assets/index.android.bundle
// replaced and expo-updates disabled in AndroidManifest.xml (same
// compression methods), META-INF v1 signature files dropped and every
// other entry copied raw (stored entries stay stored, so resources.arsc
// and native libs keep their uncompressed layout). updatesOff reports
// whether the manifest carried expo-updates' ENABLED flag.
func swapAPKBundle(src, bundlePath, dst string) (updatesOff bool, err error) {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return false, fmt.Errorf("open %s: %w", src, err)
	}
	defer zr.Close()
	bundle, err := os.ReadFile(bundlePath)
	if err != nil {
		return false, err
	}
	f, err := os.Create(dst)
	if err != nil {
		return false, err
	}
	zw := zip.NewWriter(f)
	replaced := false
	for _, e := range zr.File {
		switch {
		case isV1Signature(e.Name):
			continue
		case e.Name == "assets/index.android.bundle":
			if err := writeEntry(zw, e.FileHeader, bundle); err != nil {
				return false, closeAll(err, zw, f)
			}
			replaced = true
			continue
		case e.Name == "AndroidManifest.xml":
			manifest, err := readEntry(e)
			if err != nil {
				return false, closeAll(err, zw, f)
			}
			patched, found, err := disableExpoUpdates(manifest)
			if err != nil {
				return false, closeAll(fmt.Errorf("%s AndroidManifest.xml: %w", src, err), zw, f)
			}
			if found {
				if err := writeEntry(zw, e.FileHeader, patched); err != nil {
					return false, closeAll(err, zw, f)
				}
				updatesOff = true
				continue
			}
		}
		raw, err := e.OpenRaw()
		if err != nil {
			return false, closeAll(err, zw, f)
		}
		hdr := e.FileHeader
		w, err := zw.CreateRaw(&hdr)
		if err != nil {
			return false, closeAll(err, zw, f)
		}
		if _, err := io.Copy(w, raw); err != nil {
			return false, closeAll(err, zw, f)
		}
	}
	if !replaced {
		_ = closeAll(nil, zw, f)
		return false, diag(DiagBundleNotHBC, src+" embeds no assets/index.android.bundle to replace",
			"build the native APK as a Release build (perflab build native --platform android --profile <profile> --kind bundled --json)")
	}
	if err := zw.Close(); err != nil {
		_ = f.Close()
		return false, err
	}
	return updatesOff, f.Close()
}

func readEntry(e *zip.File) ([]byte, error) {
	rc, err := e.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// writeEntry writes data under the old entry's header. A stored entry (the
// HBC is stored so Hermes can mmap it) is written raw with its CRC and
// sizes up front: no data descriptor, as the build wrote it.
func writeEntry(zw *zip.Writer, old zip.FileHeader, data []byte) error {
	hdr := old
	hdr.Extra = nil
	if hdr.Method != zip.Store {
		hdr.CompressedSize64, hdr.UncompressedSize64, hdr.CRC32 = 0, 0, 0
		w, err := zw.CreateHeader(&hdr)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	}
	hdr.CRC32 = crc32.ChecksumIEEE(data)
	hdr.CompressedSize64, hdr.UncompressedSize64 = uint64(len(data)), uint64(len(data))
	hdr.Flags &^= 0x8
	w, err := zw.CreateRaw(&hdr)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

func closeAll(err error, zw *zip.Writer, f *os.File) error {
	_ = zw.Close()
	_ = f.Close()
	return err
}

// isV1Signature names the JAR signature files apksigner rewrites.
func isV1Signature(name string) bool {
	if !strings.HasPrefix(name, "META-INF/") || strings.Count(name, "/") != 1 {
		return false
	}
	base := strings.ToUpper(strings.TrimPrefix(name, "META-INF/"))
	if base == "MANIFEST.MF" {
		return true
	}
	for _, ext := range []string{".SF", ".RSA", ".DSA", ".EC"} {
		if strings.HasSuffix(base, ext) {
			return true
		}
	}
	return false
}

var (
	resourceLine = regexp.MustCompile(`^\s*resource 0x[0-9a-f]+ ([a-z]+)/(\S+)`)
	configLine   = regexp.MustCompile(`^\s*\(([^)]*)\) \(file\) (\S+)`)
	versionQual  = regexp.MustCompile(`-v\d+$`)
)

// apkResources maps "type/name|config" to the file path inside the APK,
// read from `aapt2 dump resources`.
func apkResources(out []byte) map[string]string {
	res := map[string]string{}
	var cur string
	for _, line := range strings.Split(string(out), "\n") {
		if m := resourceLine.FindStringSubmatch(line); m != nil {
			cur = m[1] + "/" + m[2]
			continue
		}
		if m := configLine.FindStringSubmatch(line); m != nil && cur != "" {
			res[cur+"|"+versionQual.ReplaceAllString(m[1], "")] = m[2]
		}
	}
	return res
}

// assetResourceKey maps an exported Android asset (drawable-mdpi/x.png,
// raw/y.json) to its resource key "drawable/x|mdpi".
func assetResourceKey(rel string) (string, bool) {
	dir, file, ok := strings.Cut(filepath.ToSlash(rel), "/")
	if !ok || strings.Contains(file, "/") {
		return "", false
	}
	typ, config, _ := strings.Cut(dir, "-")
	name := file
	if i := strings.Index(name, "."); i >= 0 {
		name = name[:i]
	}
	return typ + "/" + name + "|" + versionQual.ReplaceAllString(config, ""), true
}

// compiledAsset reports whether aapt2 rewrites the file into the APK
// (PNG crunching, XML compiled to binary XML), so its bytes there never
// equal the exported source.
func compiledAsset(rel string) bool {
	lower := strings.ToLower(rel)
	return strings.HasSuffix(lower, ".png") || strings.HasSuffix(lower, ".xml")
}

// checkAndroidAssets refuses a bundle whose assets the APK lacks (by
// resource name and density) or whose bytes changed. Content is compared
// against the source digests the engine's Gradle recipe recorded
// (BuildManifest.AndroidAssets); without them (an imported APK) only the
// verbatim types are compared against the APK and the compiled ones
// (PNG, XML) are reported in an info diagnostic as unverified.
func checkAndroidAssets(ctx context.Context, r Runner, b Build, bundle Bundle) ([]runx.Diagnostic, error) {
	dir := filepath.Join(bundle.Dir, "assets")
	if !fileExists(dir) {
		return nil, nil
	}
	aapt2, err := androidBuildTool("aapt2")
	if err != nil {
		return nil, err
	}
	res, err := run(ctx, r, Cmd{Argv: []string{aapt2, "dump", "resources", b.Path}, Timeout: 2 * time.Minute})
	if err != nil {
		return nil, err
	}
	if res.Exit != 0 {
		return nil, fmt.Errorf("aapt2 dump resources %s exited %d: %s", b.Path, res.Exit, lastLines(res.Stderr, 2))
	}
	have := apkResources(res.Stdout)
	zr, err := zip.OpenReader(b.Path)
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	entries := map[string]*zip.File{}
	for _, e := range zr.File {
		entries[e.Name] = e
	}
	recorded := b.Manifest.AndroidAssets
	var missing, changed, unverified []string
	err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		rel = filepath.ToSlash(rel)
		key, ok := assetResourceKey(rel)
		inAPK, has := have[key]
		if !ok || !has {
			missing = append(missing, rel)
			return nil
		}
		if recorded != nil {
			want, ok := recorded[rel]
			if !ok {
				missing = append(missing, rel)
				return nil
			}
			got, err := fileSHA256(path)
			if err != nil {
				return err
			}
			if got != want {
				changed = append(changed, rel)
			}
			return nil
		}
		if compiledAsset(rel) {
			unverified = append(unverified, rel)
			return nil
		}
		e := entries[inAPK]
		if e == nil {
			missing = append(missing, rel)
			return nil
		}
		same, err := sameContent(path, e)
		if err != nil {
			return err
		}
		if !same {
			changed = append(changed, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var diags []runx.Diagnostic
	if len(unverified) > 0 {
		sort.Strings(unverified)
		diags = append(diags, info(DiagBundleAssetsChanged,
			fmt.Sprintf("%d PNG/XML asset(s) exist in the APK by name and density, but aapt2 compiled them, so their content was not compared (%s); an imported APK records no source digests",
				len(unverified), listHead(unverified, 2)),
			nativeFor(bundle.Manifest, KindBundled)+" (an engine build records them)"))
	}
	if len(missing)+len(changed) == 0 {
		return diags, nil
	}
	sort.Strings(missing)
	sort.Strings(changed)
	var parts []string
	if len(missing) > 0 {
		parts = append(parts, fmt.Sprintf("%d asset(s) the APK has no resource for (%s)", len(missing), listHead(missing, 4)))
	}
	if len(changed) > 0 {
		parts = append(parts, fmt.Sprintf("%d asset(s) whose bytes differ from the native build's (%s)", len(changed), listHead(changed, 4)))
	}
	return nil, diag(DiagBundleAssetsChanged,
		"bundle "+bundle.Manifest.SHA256[:12]+" ships "+strings.Join(parts, " and ")+"; a swapped APK would show the old pixels or none",
		nativeFor(bundle.Manifest, KindBundled))
}

func sameContent(path string, e *zip.File) (bool, error) {
	st, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	if uint64(st.Size()) != e.UncompressedSize64 {
		return false, nil
	}
	want, err := fileSHA256(path)
	if err != nil {
		return false, err
	}
	rc, err := e.Open()
	if err != nil {
		return false, err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return false, err
	}
	return sha256Hex(b) == want, nil
}

func listHead(xs []string, n int) string {
	if len(xs) <= n {
		return strings.Join(xs, ", ")
	}
	return strings.Join(xs[:n], ", ") + fmt.Sprintf(", +%d more", len(xs)-n)
}

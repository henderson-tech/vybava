package buildindex

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// packIOS is the swap proven in the calendar campaign
// (/tmp/calendar-perf/install-variant.sh): copy the .app, keep its
// entitlements, drop in main.jsbundle and the assets (assets-dest mirrors
// the app root), turn expo-updates off (a SKIP_BUNDLING build has no
// embedded manifest and aborts at launch; both variants carry it, so an
// A/B stays fair), stamp CFBundleVersion for fencing, re-sign with the
// original identity and verify.
func packIOS(ctx context.Context, r Runner, b Build, bundle Bundle, stage string, vm *VariantManifest, progress *Progress) error {
	app := filepath.Join(stage, filepath.Base(b.Path))
	step := func(phase string, argv ...string) (Result, error) {
		res, err := run(ctx, r, Cmd{Argv: argv, Timeout: 5 * time.Minute})
		if err != nil {
			return res, err
		}
		if res.Exit != 0 {
			return res, fmt.Errorf("%s: %s exited %d: %s", phase, argv[0], res.Exit, lastLines(res.Stderr, 3))
		}
		return res, nil
	}
	phase(progress, "copy")
	if _, err := step("copy", "ditto", b.Path, app); err != nil {
		return err
	}
	res, err := step("entitlements", "codesign", "-d", "--entitlements", "-", "--xml", app)
	if err != nil || len(strings.TrimSpace(string(res.Stdout))) == 0 {
		return diag(DiagResignFailed, "the native app's entitlements cannot be read (codesign -d --entitlements - --xml)",
			"perflab build list --json, then perflab build native for that key (the indexed app lost its signature)")
	}
	ent := filepath.Join(stage, "entitlements.plist")
	if err := os.WriteFile(ent, res.Stdout, 0o644); err != nil {
		return err
	}
	phase(progress, "swap")
	if err := copyFile(bundle.Path, filepath.Join(app, "main.jsbundle")); err != nil {
		return err
	}
	if assets := filepath.Join(bundle.Dir, "assets"); fileExists(assets) {
		if err := copyTree(assets, app); err != nil {
			return err
		}
	}
	if expo := filepath.Join(app, "Expo.plist"); fileExists(expo) {
		if _, err := step("expo-updates", "plutil", "-replace", "EXUpdatesEnabled", "-bool", "NO", expo); err != nil {
			return err
		}
		vm.ExpoUpdatesDisabled = true
	}
	info, err := iosAppInfo(ctx, r, app)
	if err != nil {
		return err
	}
	vm.OriginalBundleVersion = info.BuildNumber
	vm.BundleVersion = bundleVersionStamp(info.BuildNumber, vm.VariantID)
	if _, err := step("stamp", "plutil", "-replace", "CFBundleVersion", "-string", vm.BundleVersion, filepath.Join(app, "Info.plist")); err != nil {
		return err
	}
	phase(progress, "resign")
	signing, err := iosSigningOf(ctx, r, app, stage)
	if err != nil {
		return err
	}
	ids, err := codesigningIdentities(ctx, r)
	if err != nil {
		return err
	}
	if !hasIdentity(ids, signing.IdentitySHA1) {
		return diag(DiagSigningIdentityMissing,
			"the identity that signed the native build ("+signing.IdentitySHA1+", team "+signing.Team+") is not a valid code-signing identity in this keychain",
			"security find-identity -v -p codesigning (sign in to the team in Xcode > Settings > Accounts), or rebuild natively here")
	}
	vm.IdentitySHA1 = signing.IdentitySHA1
	res, err = run(ctx, r, Cmd{Argv: []string{"codesign", "--force", "--sign", signing.IdentitySHA1, "--entitlements", ent, "--generate-entitlement-der", app}, Timeout: 5 * time.Minute})
	if err != nil {
		return err
	}
	if res.Exit != 0 {
		return diag(DiagResignFailed, "codesign refused the variant: "+lastLines(res.Stderr, 3),
			"security find-identity -v -p codesigning (the identity must have its private key)")
	}
	res, err = run(ctx, r, Cmd{Argv: []string{"codesign", "--verify", "--deep", "--strict", app}, Timeout: 5 * time.Minute})
	if err != nil {
		return err
	}
	if res.Exit != 0 {
		return diag(DiagVerifyFailed, "the re-signed variant does not verify: "+lastLines(res.Stderr, 3),
			"codesign --verify --deep --strict --verbose=2 "+app)
	}
	_ = os.Remove(ent)
	vm.Artifact = filepath.Base(app)
	return nil
}

func hasIdentity(ids []identity, sha1 string) bool {
	for _, id := range ids {
		if id.SHA1 == sha1 {
			return true
		}
	}
	return false
}

// bundleVersionStamp is `<first component of the build number>.<n>`, n a
// decimal of the variant id's hash: CFBundleVersion must stay up to three
// period-separated integers, and fencing needs one value per variant.
func bundleVersionStamp(original, variantID string) string {
	major := strings.SplitN(original, ".", 2)[0]
	if _, err := strconv.Atoi(major); err != nil || major == "" {
		major = "1"
	}
	n, _ := strconv.ParseUint(sha256Hex([]byte(variantID))[:6], 16, 32)
	return fmt.Sprintf("%s.%d", major, n)
}

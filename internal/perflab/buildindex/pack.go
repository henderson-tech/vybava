package buildindex

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/henderson-tech/vybava/internal/runx"
)

// PackSpec is one `perflab pack --native <key> --bundle <sha>`.
type PackSpec struct {
	NativeKey string
	BundleSHA string
	Progress  *Progress
}

// PackResult is the `pack` payload.
type PackResult struct {
	Hit         bool              `json:"hit"`
	Variant     Variant           `json:"variant"`
	Diagnostics []runx.Diagnostic `json:"-"`
	Next        []string          `json:"-"`
}

// Pack puts a stored bundle into a copy of a native build: content
// addressed (variants/<key12>-<sha12>/), never rebuilt in place. It refuses
// a bundle exported under another native source key.
func (s Store) Pack(ctx context.Context, r Runner, spec PackSpec) (PackResult, error) {
	if spec.NativeKey == "" || spec.BundleSHA == "" {
		return PackResult{}, diag(DiagUsage, "pack needs a native key and a bundle sha", "perflab pack --native <key> --bundle <sha> --json")
	}
	b, err := s.BuildByKey(spec.NativeKey)
	if err != nil {
		return PackResult{}, err
	}
	bundle, err := s.FindBundle(spec.BundleSHA)
	if err != nil {
		return PackResult{}, err
	}
	bm, nm := bundle.Manifest, b.Manifest
	if bm.Platform != nm.Platform {
		return PackResult{}, diag(DiagUsage, fmt.Sprintf("bundle %s is %s, native build %s is %s", bm.SHA256[:12], bm.Platform, nm.Key, nm.Platform),
			fmt.Sprintf("perflab bundle list --platform %s --json", nm.Platform))
	}
	if bm.SourceKey != nm.SourceKey {
		return PackResult{}, diag(DiagFingerprintMismatch,
			fmt.Sprintf("bundle %s was exported from native sources %s, build %s was built from %s; its JS would run against other native modules or Hermes",
				bm.SHA256[:12], bm.SourceKey, nm.Key, nm.SourceKey),
			nativeFor(bm, nm.Kind))
	}
	id := VariantID(nm.Key, bm.SHA256)
	notEquivalent := info(DiagNotProductionEquivalent,
		"a packed variant runs its bundle with expo-updates bypassed; compare variants with each other, not with the store build",
		fmt.Sprintf("perflab build native --platform %s --profile %s --kind bundled --json (the as-shipped confirmation)", nm.Platform, nm.Profile))
	next := []string{fmt.Sprintf("perflab install %s --device <id> --lease <token> --json", id)}
	if v, err := s.FindVariant(id); err == nil {
		return PackResult{Hit: true, Variant: v, Diagnostics: []runx.Diagnostic{notEquivalent}, Next: next}, nil
	}
	st, err := newStage(filepath.Join(s.Dirs.variantsDir(), id))
	if err != nil {
		return PackResult{}, err
	}
	vm := VariantManifest{
		VariantID: id, Platform: nm.Platform, AppID: nm.AppID, NativeKey: nm.Key, BundleSHA: bm.SHA256,
		BundleLabel: bm.Label, PackedAt: time.Now().UTC(),
	}
	diags := []runx.Diagnostic{notEquivalent}
	if nm.Platform == "ios" {
		err = packIOS(ctx, r, b, bundle, st.Dir, &vm, spec.Progress)
	} else {
		err = packAndroid(ctx, r, b, bundle, st.Dir, &vm, spec.Progress, &diags)
	}
	if err != nil {
		st.discard()
		return PackResult{}, err
	}
	if err := writeJSONAtomic(filepath.Join(st.Dir, manifestFile), vm); err != nil {
		st.discard()
		return PackResult{}, err
	}
	existed, err := st.commit()
	if err != nil {
		return PackResult{}, err
	}
	v, err := s.FindVariant(id)
	if err != nil {
		return PackResult{}, err
	}
	return PackResult{Hit: existed, Variant: v, Diagnostics: diags, Next: next}, nil
}

func short(commit string) string {
	if len(commit) > 10 {
		return commit[:10]
	}
	return commit
}

// nativeFor is the build that gives a bundle a native build of its own:
// isolated at the bundle's commit, or in its worktree when it was dirty.
func nativeFor(bm BundleManifest, kind string) string {
	t := Target{Platform: bm.Platform, Profile: bm.Profile, Kind: kind}
	if bm.Source.Commit == "" || bm.Source.Dirty {
		return t.invocation() + " (run in the bundle's worktree " + bm.Source.Worktree + ")"
	}
	return t.invocation() + " --isolated --ref " + bm.Source.Commit
}

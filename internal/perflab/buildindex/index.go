package buildindex

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The content-addressed store under the cache dir:
//
//	builds/<platform>/<profile>/<kind>/<key>/   manifest.json key-lines.txt build.log <artifact>
//	bundles/<sha16>/                           manifest.json export.log <bundle file> assets/
//	variants/<key12>-<sha12>/                  manifest.json <artifact>
//
// Entries are written into a `.tmp-*` sibling and renamed in; an existing
// entry is never overwritten (the second writer's copy is discarded and the
// first one answers). Every lookup tolerates a deleted artifact.

// BuildManifest is builds/.../<key>/manifest.json.
type BuildManifest struct {
	Key                  string         `json:"key"`
	SourceKey            string         `json:"sourceKey"`
	ExpoHash             string         `json:"expoHash"`
	Platform             string         `json:"platform"`
	Profile              string         `json:"profile"`
	Kind                 string         `json:"kind"`
	AppID                string         `json:"appId"`
	Version              string         `json:"version,omitempty"`
	BuildNumber          string         `json:"buildNumber,omitempty"`
	Signing              Signing        `json:"signing"`
	Toolchain            Toolchain      `json:"toolchain"`
	CreatedAt            time.Time      `json:"createdAt"`
	Source               SourceIdentity `json:"source"`
	ProductionEquivalent bool           `json:"productionEquivalent"`
	PublicEnvHash        string         `json:"publicEnvHash,omitempty"`
	EnvNames             []string       `json:"envNames"`
	BuildLog             string         `json:"buildLog,omitempty"`
	DurationMs           int64          `json:"durationMs"`
	Imported             bool           `json:"imported,omitempty"`
	ImportedFrom         string         `json:"importedFrom,omitempty"`
	Artifact             string         `json:"artifact"`
	ArtifactSHA256       string         `json:"artifactSha256,omitempty"`
	// AndroidAssets are the sha256 of every app asset Gradle generated
	// (build/generated/res/react/release, before aapt2 compiles PNG and XML),
	// by path relative to it; pack compares a bundle's assets against them.
	AndroidAssets map[string]string `json:"androidAssets,omitempty"`
	// Keystore is the template debug keystore copied beside an Android
	// artifact so pack can re-sign without the source checkout.
	Keystore string `json:"keystore,omitempty"`
}

// BundleManifest is bundles/<sha16>/manifest.json.
type BundleManifest struct {
	SHA256        string         `json:"sha256"`
	Platform      string         `json:"platform"`
	Profile       string         `json:"profile"`
	Label         string         `json:"label,omitempty"`
	SourceKey     string         `json:"sourceKey"`
	Source        SourceIdentity `json:"source"`
	Ref           string         `json:"ref,omitempty"`
	PublicEnvHash string         `json:"publicEnvHash,omitempty"`
	EnvNames      []string       `json:"envNames"`
	Modules       int            `json:"modules,omitempty"`
	AssetsCount   int            `json:"assetsCount"`
	AssetsDigest  string         `json:"assetsDigest"`
	HBC           bool           `json:"hbc"`
	HBCVersion    uint32         `json:"hbcVersion"`
	File          string         `json:"file"`
	CreatedAt     time.Time      `json:"createdAt"`
	DurationMs    int64          `json:"durationMs"`
}

// VariantManifest is variants/<id>/manifest.json.
type VariantManifest struct {
	VariantID            string    `json:"variantId"`
	Platform             string    `json:"platform"`
	AppID                string    `json:"appId"`
	NativeKey            string    `json:"nativeKey"`
	BundleSHA            string    `json:"bundleSha"`
	BundleLabel          string    `json:"bundleLabel,omitempty"`
	ProductionEquivalent bool      `json:"productionEquivalent"`
	PackedAt             time.Time `json:"packedAt"`
	Artifact             string    `json:"artifact"`
	// BundleVersion is the iOS CFBundleVersion stamp pack wrote (fencing).
	BundleVersion string `json:"bundleVersion,omitempty"`
	// OriginalBundleVersion is the native build's CFBundleVersion.
	OriginalBundleVersion string `json:"originalBundleVersion,omitempty"`
	// IdentitySHA1 is the identity the iOS variant was re-signed with.
	IdentitySHA1 string `json:"identitySha1,omitempty"`
	// ExpoUpdatesDisabled: pack turned expo-updates off in the variant
	// (iOS EXUpdatesEnabled=NO, Android the ENABLED meta-data), so no OTA
	// update can replace the swapped bundle.
	ExpoUpdatesDisabled bool `json:"expoUpdatesDisabled,omitempty"`
	// ArtifactSHA256 is the Android variant APK's sha256 (fencing).
	ArtifactSHA256 string `json:"artifactSha256,omitempty"`
}

// Build is one indexed native build.
type Build struct {
	Dir      string        `json:"dir"`
	Path     string        `json:"path"`
	Manifest BuildManifest `json:"manifest"`
}

// Bundle is one stored JS bundle.
type Bundle struct {
	Dir      string         `json:"dir"`
	Path     string         `json:"path"`
	Manifest BundleManifest `json:"manifest"`
}

// Variant is one packed native build + bundle.
type Variant struct {
	Dir      string          `json:"dir"`
	Path     string          `json:"path"`
	Manifest VariantManifest `json:"manifest"`
}

// Store is the index rooted at Dirs.Cache.
type Store struct {
	Dirs Dirs
}

const manifestFile = "manifest.json"

func (s Store) buildDir(t Target, key string) string {
	return filepath.Join(s.Dirs.buildsDir(), t.Platform, t.Profile, t.Kind, key)
}

// FindBuild looks the key up. A miss is BUILD_NOT_FOUND; an entry whose
// artifact was deleted is removed and answers BUILD_ARTIFACT_MISSING.
func (s Store) FindBuild(t Target, key string) (Build, error) {
	dir := s.buildDir(t, key)
	b, err := readBuild(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return Build{}, diag(DiagBuildNotFound,
			fmt.Sprintf("no %s %s %s build carries key %s", t.Platform, t.Profile, t.Kind, key),
			fmt.Sprintf("perflab build native --platform %s --profile %s --kind %s --json", t.Platform, t.Profile, t.Kind))
	}
	if err != nil {
		return Build{}, err
	}
	if !fileExists(b.Path) {
		_ = os.RemoveAll(dir)
		return Build{}, diag(DiagBuildArtifactMissing,
			fmt.Sprintf("the index entry %s lost its artifact %s; the entry was dropped", key, b.Manifest.Artifact),
			fmt.Sprintf("perflab build native --platform %s --profile %s --kind %s --json", t.Platform, t.Profile, t.Kind))
	}
	touchUsed(dir)
	return b, nil
}

// BuildByKey finds a native build by key in any platform/profile/kind.
func (s Store) BuildByKey(key string) (Build, error) {
	matches, _ := filepath.Glob(filepath.Join(s.Dirs.buildsDir(), "*", "*", "*", key, manifestFile))
	if len(matches) == 0 {
		return Build{}, diag(DiagBuildNotFound, "no indexed native build carries key "+key, "perflab build list --json")
	}
	dir := filepath.Dir(matches[0])
	b, err := readBuild(dir)
	if err != nil {
		return Build{}, err
	}
	if !fileExists(b.Path) {
		_ = os.RemoveAll(dir)
		return Build{}, diag(DiagBuildArtifactMissing, "the index entry "+key+" lost its artifact; the entry was dropped",
			fmt.Sprintf("perflab build native --platform %s --profile %s --kind %s --json", b.Manifest.Platform, b.Manifest.Profile, b.Manifest.Kind))
	}
	touchUsed(dir)
	return b, nil
}

func readBuild(dir string) (Build, error) {
	var m BuildManifest
	if err := readJSON(filepath.Join(dir, manifestFile), &m); err != nil {
		return Build{}, err
	}
	return Build{Dir: dir, Path: filepath.Join(dir, m.Artifact), Manifest: m}, nil
}

// ListBuilds lists every build entry, newest first.
func (s Store) ListBuilds() ([]Build, error) {
	matches, err := filepath.Glob(filepath.Join(s.Dirs.buildsDir(), "*", "*", "*", "*", manifestFile))
	if err != nil {
		return nil, err
	}
	out := []Build{}
	for _, m := range matches {
		if b, err := readBuild(filepath.Dir(m)); err == nil {
			out = append(out, b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Manifest.CreatedAt.After(out[j].Manifest.CreatedAt) })
	return out, nil
}

// FindBundle resolves a bundle by sha256 prefix (at least 12 hex).
func (s Store) FindBundle(sha string) (Bundle, error) {
	sha = strings.ToLower(sha)
	if len(sha) < 12 {
		return Bundle{}, diag(DiagUsage, "bundle sha "+sha+" is shorter than 12 hex", "perflab bundle list --json")
	}
	dir := filepath.Join(s.Dirs.bundlesDir(), sha[:min(len(sha), 16)])
	var m BundleManifest
	if err := readJSON(filepath.Join(dir, manifestFile), &m); err != nil || !strings.HasPrefix(m.SHA256, sha) {
		// a 12..15 hex prefix: scan
		all, _ := s.ListBundles()
		var hits []Bundle
		for _, b := range all {
			if strings.HasPrefix(b.Manifest.SHA256, sha) {
				hits = append(hits, b)
			}
		}
		if len(hits) != 1 {
			return Bundle{}, diag(DiagUsage, fmt.Sprintf("bundle %s matches %d stored bundles", sha, len(hits)), "perflab bundle list --json")
		}
		return hits[0], nil
	}
	b := Bundle{Dir: dir, Path: filepath.Join(dir, m.File), Manifest: m}
	if !fileExists(b.Path) {
		_ = os.RemoveAll(dir)
		return Bundle{}, diag(DiagBuildArtifactMissing, "the stored bundle "+sha+" lost its file; the entry was dropped",
			fmt.Sprintf("perflab bundle export --platform %s --profile %s --json", m.Platform, m.Profile))
	}
	touchUsed(dir)
	return b, nil
}

// ListBundles lists stored bundles, newest first.
func (s Store) ListBundles() ([]Bundle, error) {
	matches, err := filepath.Glob(filepath.Join(s.Dirs.bundlesDir(), "*", manifestFile))
	if err != nil {
		return nil, err
	}
	out := []Bundle{}
	for _, mf := range matches {
		var m BundleManifest
		if readJSON(mf, &m) == nil {
			dir := filepath.Dir(mf)
			out = append(out, Bundle{Dir: dir, Path: filepath.Join(dir, m.File), Manifest: m})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Manifest.CreatedAt.After(out[j].Manifest.CreatedAt) })
	return out, nil
}

// VariantID is <key12>-<sha12>: the native key's first 12 characters and
// the bundle sha256's first 12 hex.
func VariantID(nativeKey, bundleSHA string) string {
	return nativeKey[:min(12, len(nativeKey))] + "-" + bundleSHA[:min(12, len(bundleSHA))]
}

// FindVariant reads a packed variant.
func (s Store) FindVariant(id string) (Variant, error) {
	dir := filepath.Join(s.Dirs.variantsDir(), id)
	var m VariantManifest
	if err := readJSON(filepath.Join(dir, manifestFile), &m); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Variant{}, diag(DiagBuildNotFound, "no packed variant "+id, "perflab pack --native <key> --bundle <sha> --json")
		}
		return Variant{}, err
	}
	v := Variant{Dir: dir, Path: filepath.Join(dir, m.Artifact), Manifest: m}
	if !fileExists(v.Path) {
		_ = os.RemoveAll(dir)
		return Variant{}, diag(DiagBuildArtifactMissing, "the variant "+id+" lost its artifact; the entry was dropped",
			fmt.Sprintf("perflab pack --native %s --bundle %s --json", m.NativeKey, m.BundleSHA))
	}
	touchUsed(dir)
	return v, nil
}

// ListVariants lists packed variants, newest first.
func (s Store) ListVariants() ([]Variant, error) {
	matches, err := filepath.Glob(filepath.Join(s.Dirs.variantsDir(), "*", manifestFile))
	if err != nil {
		return nil, err
	}
	out := []Variant{}
	for _, mf := range matches {
		var m VariantManifest
		if readJSON(mf, &m) == nil {
			dir := filepath.Dir(mf)
			out = append(out, Variant{Dir: dir, Path: filepath.Join(dir, m.Artifact), Manifest: m})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Manifest.PackedAt.After(out[j].Manifest.PackedAt) })
	return out, nil
}

// Installable is what `perflab install` puts on a device: a packed variant
// or a native build installed as is.
type Installable struct {
	ID                   string         `json:"id"`
	Platform             string         `json:"platform"`
	AppID                string         `json:"appId"`
	Path                 string         `json:"path"`
	ProductionEquivalent bool           `json:"productionEquivalent"`
	Stamp                InstalledStamp `json:"stamp"`
}

// InstalledStamp is what fencing compares on the device: the APK sha256 on
// Android, the CFBundleVersion on iOS.
type InstalledStamp struct {
	Platform      string `json:"platform"`
	AppID         string `json:"appId"`
	APKSHA256     string `json:"apkSha256,omitempty"`
	BundleVersion string `json:"bundleVersion,omitempty"`
}

// Resolve maps a variant id or a native key to its installable artifact.
func (s Store) Resolve(id string) (Installable, error) {
	if strings.Count(id, "-") >= 2 && fileExists(filepath.Join(s.Dirs.variantsDir(), id, manifestFile)) {
		v, err := s.FindVariant(id)
		if err != nil {
			return Installable{}, err
		}
		m := v.Manifest
		return Installable{
			ID: id, Platform: m.Platform, AppID: m.AppID, Path: v.Path, ProductionEquivalent: m.ProductionEquivalent,
			Stamp: InstalledStamp{Platform: m.Platform, AppID: m.AppID, APKSHA256: m.ArtifactSHA256, BundleVersion: m.BundleVersion},
		}, nil
	}
	b, err := s.BuildByKey(id)
	if err != nil {
		if d, ok := asDiag(err); ok && d.Diag.Code == DiagBuildNotFound {
			return Installable{}, diag(DiagBuildNotFound, "no variant or native build is called "+id,
				"perflab build list --json (native keys) or perflab pack --native <key> --bundle <sha> --json")
		}
		return Installable{}, err
	}
	m := b.Manifest
	return Installable{
		ID: id, Platform: m.Platform, AppID: m.AppID, Path: b.Path, ProductionEquivalent: m.ProductionEquivalent,
		Stamp: InstalledStamp{Platform: m.Platform, AppID: m.AppID, APKSHA256: m.ArtifactSHA256, BundleVersion: m.BuildNumber},
	}, nil
}

// GCResult is `perflab build gc`'s payload.
type GCResult struct {
	Removed   []string `json:"removed"`
	Kept      int      `json:"kept"`
	Pinned    []string `json:"pinned"`
	FreedMB   int64    `json:"freedMb"`
	TotalMB   int64    `json:"totalMb"`
	MaxSizeMB int64    `json:"maxSizeMb,omitempty"`
}

type gcEntry struct {
	id       string
	dir      string
	group    string
	lastUsed time.Time
	size     int64
}

// GC keeps the newest keep entries per build target (and per variant
// native key), then removes the least recently used until the cache fits
// maxSize (0 = no cap). pinned names variant ids and native keys a live
// lease's lastInstalled references; they are never removed, nor is the
// native build a pinned variant was packed from.
func (s Store) GC(keep int, maxSize int64, pinned []string, dryRun bool) (GCResult, error) {
	pin := map[string]bool{}
	for _, p := range pinned {
		pin[p] = true
	}
	variants, err := s.ListVariants()
	if err != nil {
		return GCResult{}, err
	}
	for _, v := range variants {
		if pin[v.Manifest.VariantID] {
			pin[v.Manifest.NativeKey] = true
		}
	}
	builds, err := s.ListBuilds()
	if err != nil {
		return GCResult{}, err
	}
	var entries []gcEntry
	for _, b := range builds {
		m := b.Manifest
		entries = append(entries, gcEntry{id: m.Key, dir: b.Dir, group: "build/" + m.Platform + "/" + m.Profile + "/" + m.Kind, lastUsed: lastUsed(b.Dir, m.CreatedAt), size: dirSize(b.Dir)})
	}
	for _, v := range variants {
		m := v.Manifest
		entries = append(entries, gcEntry{id: m.VariantID, dir: v.Dir, group: "variant/" + m.NativeKey, lastUsed: lastUsed(v.Dir, m.PackedAt), size: dirSize(v.Dir)})
	}
	bundles, err := s.ListBundles()
	if err != nil {
		return GCResult{}, err
	}
	for _, b := range bundles {
		m := b.Manifest
		entries = append(entries, gcEntry{id: m.SHA256[:16], dir: b.Dir, group: "bundle/" + m.Platform + "/" + m.Profile, lastUsed: lastUsed(b.Dir, m.CreatedAt), size: dirSize(b.Dir)})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].lastUsed.After(entries[j].lastUsed) })

	res := GCResult{Removed: []string{}, Pinned: []string{}, MaxSizeMB: maxSize >> 20}
	var total int64
	for _, e := range entries {
		total += e.size
	}
	perGroup := map[string]int{}
	var candidates []gcEntry
	for _, e := range entries {
		perGroup[e.group]++
		if pin[e.id] {
			res.Pinned = append(res.Pinned, e.id)
			continue
		}
		if perGroup[e.group] > keep {
			candidates = append(candidates, e)
		}
	}
	// over the size cap: the least recently used survivors go next
	if maxSize > 0 {
		left := total
		for _, c := range candidates {
			left -= c.size
		}
		for i := len(entries) - 1; i >= 0 && left > maxSize; i-- {
			e := entries[i]
			if pin[e.id] || containsEntry(candidates, e.dir) {
				continue
			}
			candidates = append(candidates, e)
			left -= e.size
		}
	}
	for _, c := range candidates {
		if !dryRun {
			if err := os.RemoveAll(c.dir); err != nil {
				return res, err
			}
		}
		res.Removed = append(res.Removed, c.id)
		res.FreedMB += c.size >> 20
	}
	res.Kept = len(entries) - len(candidates)
	res.TotalMB = total >> 20
	if !dryRun {
		s.sweepStaging(6 * time.Hour)
	}
	return res, nil
}

// sweepStaging removes `.tmp-*` staging dirs a crashed writer left behind,
// once they are older than age (a live writer's stage is younger).
func (s Store) sweepStaging(age time.Duration) {
	for _, pattern := range []string{
		filepath.Join(s.Dirs.buildsDir(), "*", "*", "*", ".tmp-*"),
		filepath.Join(s.Dirs.bundlesDir(), ".tmp-*"),
		filepath.Join(s.Dirs.variantsDir(), ".tmp-*"),
	} {
		matches, _ := filepath.Glob(pattern)
		for _, m := range matches {
			if st, err := os.Stat(m); err == nil && time.Since(st.ModTime()) > age {
				_ = os.RemoveAll(m)
			}
		}
	}
}

func containsEntry(es []gcEntry, dir string) bool {
	for _, e := range es {
		if e.dir == dir {
			return true
		}
	}
	return false
}

const usedMarker = ".used"

// touchUsed bumps an entry's last-used time (GC orders by it).
func touchUsed(dir string) {
	p := filepath.Join(dir, usedMarker)
	now := time.Now()
	if err := os.Chtimes(p, now, now); err != nil {
		_ = os.WriteFile(p, nil, 0o644)
	}
}

func lastUsed(dir string, created time.Time) time.Time {
	if st, err := os.Stat(filepath.Join(dir, usedMarker)); err == nil && st.ModTime().After(created) {
		return st.ModTime()
	}
	return created
}

func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// stage is a `.tmp-*` directory beside final; commit renames it into
// place unless final already exists (then the staged copy is discarded and
// existed reports true).
type stage struct {
	Dir   string
	final string
}

func newStage(final string) (*stage, error) {
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(filepath.Dir(final), ".tmp-"+filepath.Base(final)+"-")
	if err != nil {
		return nil, err
	}
	return &stage{Dir: dir, final: final}, nil
}

func (s *stage) commit() (existed bool, err error) {
	if fileExists(filepath.Join(s.final, manifestFile)) {
		_ = os.RemoveAll(s.Dir)
		return true, nil
	}
	_ = os.RemoveAll(s.final) // a manifest-less leftover is not an entry
	if err := os.Rename(s.Dir, s.final); err != nil {
		_ = os.RemoveAll(s.Dir)
		if fileExists(filepath.Join(s.final, manifestFile)) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}

func (s *stage) discard() { _ = os.RemoveAll(s.Dir) }

func asDiag(err error) (runxDiagError, bool) {
	var d runxDiagError
	ok := errors.As(err, &d)
	return d, ok
}

package reclaim

// bun-prune is the per-checkout half of the bun store. An isolated install
// (`linker = "isolated"`) keeps one node_modules/.bun/<pkg>@<ver> entry per
// resolved package, and bun never deletes one the lockfile stopped naming:
// FixIt's main checkout held 3,939 entries against ~130 real directories in a
// fresh worktree, plus a 5.2 GB .old_modules-<hash> left by a linker switch.
// Unlike the ladder this scans first, on purpose: an entry is garbage only
// when nothing in the checkout resolves to it, and only a walk can say that.
//
// Reachability: the roots are the checkout's node_modules, every workspace
// package's node_modules (root package.json "workspaces") and
// node_modules/.bun/node_modules, bun's fallback for undeclared imports. A
// symlink found there that lands in .bun/<entry> marks that entry. A marked
// entry that is a real directory (a project-local package: patched, trusted
// or dragged local by a peer) contributes its own node_modules links the same
// way, transitively. A marked entry that is a symlink into the global store
// ends the walk: links/ entries only ever point at other links/ entries.
// Only unreachable real directories (where the bytes are) and leftovers are
// deleted. bun links every lockfile package into .bun, so a fresh worktree
// already holds hundreds of unreachable symlinks; they are counted, never
// deleted.
//
// A native project builds from paths it resolved at its own install time:
// an iOS Pods project compiles every development pod from the .bun entry its
// ios/Podfile.lock names, and nothing re-points it but `pod install`. FixIt's
// main checkout had one from 2026-08-03 naming 59 entries (7.7 GB), every one
// unreachable from node_modules. Such an entry, and what it links to, is
// kept and listed apart (BunPruneReport.Native), never deleted.
//
// --apply refuses while a bun install-family process has its cwd inside the
// checkout (outside a nested checkout that is its own bun project), keeps
// every entry modified within MinAge (an install in flight writes fresh
// entries), walks again right before deleting, refuses when a directory or
// lockfile that walk read changed before the first rename (an install that
// ran start to finish during it), and renames each target into a trash
// directory first, so a later install never finds a half-deleted package
// under a name it trusts.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// BunEntryKind says what a prune candidate is.
type BunEntryKind string

const (
	// BunEntryDir is a project-local package directory in .bun.
	BunEntryDir BunEntryKind = "dir"
	// BunEntrySymlink is a .bun entry pointing into the global store. It is
	// counted, never deleted (BunPruneReport.UnreachableLinks).
	BunEntrySymlink BunEntryKind = "symlink"
	// BunEntryLeftover is node_modules/.old_modules-* (a linker switch) or a
	// trash directory an interrupted --apply left.
	BunEntryLeftover BunEntryKind = "leftover"
)

const (
	bunOldModulesPrefix = ".old_modules-"
	bunPruneTrashPrefix = ".bun-prune-trash-"
)

// BunPruneOptions steer one prune.
type BunPruneOptions struct {
	Checkout string
	Apply    bool
	// MinAge keeps candidates modified more recently (default 24 h).
	MinAge time.Duration
	Now    time.Time
	// BunCwds lists running bun processes, their working directories and
	// command lines; nil reads them with lsof and ps. Tests substitute it.
	BunCwds func(ctx context.Context) ([]BunProcess, error)
}

// BunProcess is a running bun, its working directory and its command line.
// Args is empty when ps no longer listed the pid lsof found; such a process
// counts as a writer.
type BunProcess struct {
	PID  int    `json:"pid"`
	Cwd  string `json:"cwd"`
	Args string `json:"args,omitempty"`
}

// BunPruneEntry is one candidate.
type BunPruneEntry struct {
	Name     string       `json:"name"`
	Path     string       `json:"path"`
	Kind     BunEntryKind `json:"kind"`
	Bytes    int64        `json:"bytes"`
	Modified time.Time    `json:"modified"`
}

// BunPruneReport is one prune's outcome.
type BunPruneReport struct {
	Checkout  string `json:"checkout"`
	Applied   bool   `json:"applied"`
	Entries   int    `json:"entries"`
	Reachable int    `json:"reachable"`
	// Unreachable lists the project-local directories nothing resolves to:
	// the targets, with Leftovers.
	Unreachable []BunPruneEntry `json:"unreachable"`
	// UnreachableLinks counts .bun symlinks into the global store that no
	// root reaches. Never deleted: bun links every lockfile package (a fresh
	// FixIt worktree has 569 of them), each frees nothing but a directory
	// entry, and the next install would only relink it.
	UnreachableLinks int             `json:"unreachable_links"`
	Leftovers        []BunPruneEntry `json:"leftovers"`
	// Young are candidates modified within MinAge; never deleted.
	Young []BunPruneEntry `json:"kept_young"`
	// Native are entries no node_modules root reaches but a native project's
	// lockfile (NativeManifests, checkout-relative) names, directly or through
	// a named entry's own links. Never deleted; `pod install` re-points the
	// project, and the next prune finds them unreachable. Sized in a dry run
	// (NativeBytes, outside Bytes).
	Native          []BunPruneEntry `json:"kept_native"`
	NativeManifests []string        `json:"native_manifests"`
	NativeBytes     int64           `json:"kept_native_bytes"`
	// Bytes is the candidates' logical size with hardlinks counted once (a
	// dry run), or what the delete walked (--apply). APFS clones share
	// blocks, so FreeAfter - FreeBefore is the ground truth.
	Bytes      int64    `json:"bytes"`
	FreeBefore int64    `json:"free_before,omitempty"`
	FreeAfter  int64    `json:"free_after,omitempty"`
	Errors     []string `json:"errors,omitempty"`
}

// fileID identifies an inode for hardlink dedupe.
type fileID struct{ dev, ino uint64 }

// BunPrune reports, and with Apply deletes, one checkout's unreachable .bun
// entries and node_modules leftovers.
func BunPrune(ctx context.Context, opts BunPruneOptions) (BunPruneReport, error) {
	if opts.MinAge <= 0 {
		opts.MinAge = 24 * time.Hour
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	if opts.BunCwds == nil {
		opts.BunCwds = lsofBunCwds
	}
	abs, err := filepath.Abs(opts.Checkout)
	if err != nil {
		return BunPruneReport{}, err
	}
	spellings := []string{abs}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil && resolved != abs {
		spellings = append(spellings, resolved)
	}
	checkout := spellings[len(spellings)-1]
	if opts.Apply {
		if err := refuseBusyCheckout(ctx, opts, spellings); err != nil {
			return BunPruneReport{Checkout: checkout}, err
		}
	}
	report, targets, fence, err := planBunPrune(ctx, checkout, spellings, opts, !opts.Apply)
	if err != nil || !opts.Apply {
		return report, err
	}
	// The walk takes a while on a big tree: an install may have started, or
	// started and finished (then only the fence shows it).
	if err := refuseBusyCheckout(ctx, opts, spellings); err != nil {
		return report, err
	}
	if path, moved := fence.moved(); moved {
		return report, fmt.Errorf("%s changed during the walk (an install ran?); rerun", path)
	}
	report.Applied = true
	report.FreeBefore, _, _ = Free(checkout)
	trash := filepath.Join(checkout, "node_modules", bunPruneTrashPrefix+strconv.Itoa(os.Getpid()))
	if err := os.Mkdir(trash, 0o700); err != nil && !errors.Is(err, fs.ErrExist) {
		return report, err
	}
	for i, target := range targets {
		if ctx.Err() != nil {
			break // what is not yet renamed stays whole where it was
		}
		if err := os.Rename(target, filepath.Join(trash, strconv.Itoa(i))); err != nil && !errors.Is(err, fs.ErrNotExist) {
			report.Errors = append(report.Errors, err.Error())
		}
	}
	n, err := removeTree(ctx, trash, false)
	report.Bytes = n
	if err != nil {
		report.Errors = append(report.Errors, err.Error())
	}
	report.FreeAfter, _, _ = Free(checkout)
	return report, nil
}

// planBunPrune walks the checkout and returns the report plus the paths
// --apply would delete (unreachable and leftover, none of them young), and
// the fence of what the walk read.
func planBunPrune(ctx context.Context, checkout string, spellings []string, opts BunPruneOptions, size bool) (BunPruneReport, []string, walkFence, error) {
	report := BunPruneReport{Checkout: checkout, Unreachable: []BunPruneEntry{}, Leftovers: []BunPruneEntry{}, Young: []BunPruneEntry{},
		Native: []BunPruneEntry{}, NativeManifests: []string{}}
	nm := filepath.Join(checkout, "node_modules")
	store := filepath.Join(nm, ".bun")
	names, err := bunStoreEntries(store)
	if err != nil {
		return report, nil, nil, fmt.Errorf("%s is not a bun isolated install: %w", checkout, err)
	}
	reached, manifests, fence, err := bunReachable(checkout, spellings)
	if err != nil {
		return report, nil, nil, err
	}
	report.Entries = len(names)
	report.NativeManifests = manifests
	var candidates []BunPruneEntry
	for _, name := range names {
		reach := reached[name]
		if reach == reachJS {
			report.Reachable++
			continue
		}
		path := filepath.Join(store, name)
		info, err := os.Lstat(path)
		if err != nil {
			continue // removed since the listing
		}
		kind := BunEntryDir
		if info.Mode()&fs.ModeSymlink != 0 {
			kind = BunEntrySymlink
		}
		entry := BunPruneEntry{Name: name, Path: path, Kind: kind, Modified: info.ModTime()}
		if reach == reachNative {
			report.Native = append(report.Native, entry)
			continue
		}
		candidates = append(candidates, entry)
	}
	top, err := os.ReadDir(nm)
	if err != nil {
		return report, nil, nil, err
	}
	for _, entry := range top {
		name := entry.Name()
		if !strings.HasPrefix(name, bunOldModulesPrefix) && !strings.HasPrefix(name, bunPruneTrashPrefix) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		candidates = append(candidates, BunPruneEntry{Name: name, Path: filepath.Join(nm, name), Kind: BunEntryLeftover, Modified: info.ModTime()})
	}
	seen := map[fileID]bool{}
	var targets []string
	for _, c := range candidates {
		if c.Kind == BunEntrySymlink {
			report.UnreachableLinks++
			continue
		}
		if opts.Now.Sub(c.Modified) < opts.MinAge {
			report.Young = append(report.Young, c)
			continue
		}
		if size {
			n, err := sizeDedup(ctx, c.Path, seen)
			if err != nil {
				return report, nil, nil, err
			}
			c.Bytes = n
			report.Bytes += n
		}
		targets = append(targets, c.Path)
		if c.Kind == BunEntryLeftover {
			report.Leftovers = append(report.Leftovers, c)
		} else {
			report.Unreachable = append(report.Unreachable, c)
		}
	}
	// Sized after the candidates, so a shared hardlink counts toward Bytes.
	for i := range report.Native {
		if size {
			n, err := sizeDedup(ctx, report.Native[i].Path, seen)
			if err != nil {
				return report, nil, nil, err
			}
			report.Native[i].Bytes = n
			report.NativeBytes += n
		}
	}
	sort.SliceStable(report.Unreachable, func(i, j int) bool { return report.Unreachable[i].Bytes > report.Unreachable[j].Bytes })
	return report, targets, fence, nil
}

// bunStoreEntries lists .bun's package entries: everything but the fallback
// node_modules directory and dot-files.
func bunStoreEntries(store string) ([]string, error) {
	entries, err := os.ReadDir(store)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if name := entry.Name(); name != "node_modules" && !strings.HasPrefix(name, ".") {
			names = append(names, name)
		}
	}
	return names, nil
}

// bunReach says what keeps a .bun entry.
type bunReach uint8

const (
	// reachJS: a node_modules root resolves to the entry.
	reachJS bunReach = iota + 1
	// reachNative: only a native project's lockfile does, directly or through
	// a named entry's own links.
	reachNative
)

// bunReachable returns what keeps each kept .bun entry, plus the native
// lockfiles read (checkout-relative) and the fence of everything read. The
// node_modules walk runs to the end first, so reachNative marks only what
// nothing else reaches. An unreadable root, package directory or lockfile is
// an error, never a skip: a missed root would make live entries look
// unreachable.
func bunReachable(checkout string, spellings []string) (map[string]bunReach, []string, walkFence, error) {
	dirs, err := bunPackageDirs(checkout)
	if err != nil {
		return nil, nil, nil, err
	}
	var fence walkFence
	var stores []string
	for _, s := range spellings {
		stores = append(stores, filepath.Join(s, "node_modules", ".bun"))
	}
	store := filepath.Join(checkout, "node_modules", ".bun")
	reached := map[string]bunReach{}
	var queue []string
	phase := reachJS
	enqueue := func(name string) {
		if reached[name] == 0 {
			reached[name] = phase
			queue = append(queue, name)
		}
	}
	mark := func(link string) {
		if name, ok := bunStoreTarget(stores, link); ok {
			enqueue(name)
		}
	}
	walk := func() error {
		for len(queue) > 0 {
			path := filepath.Join(store, queue[0])
			queue = queue[1:]
			info, err := os.Lstat(path)
			switch {
			case err != nil:
				continue // a dangling reference keeps nothing alive
			case info.Mode()&fs.ModeSymlink != 0:
				mark(path) // follows only when it lands back inside .bun
			case info.IsDir():
				if err := scanModuleLinks(filepath.Join(path, "node_modules"), mark, &fence); err != nil {
					return err
				}
			}
		}
		return nil
	}
	roots := []string{filepath.Join(checkout, "node_modules", ".bun", "node_modules")}
	for _, dir := range dirs {
		roots = append(roots, filepath.Join(dir, "node_modules"))
	}
	for _, root := range roots {
		if err := scanModuleLinks(root, mark, &fence); err != nil {
			return nil, nil, nil, err
		}
	}
	if err := walk(); err != nil {
		return nil, nil, nil, err
	}
	phase = reachNative
	names, manifests, err := nativeBunRefs(checkout, dirs, &fence)
	if err != nil {
		return nil, nil, nil, err
	}
	for _, name := range names {
		enqueue(name)
	}
	if err := walk(); err != nil {
		return nil, nil, nil, err
	}
	return reached, manifests, fence, nil
}

// walkFence is the mtime of every directory and lockfile the reachability
// walk read (zero: absent then). An install that starts and finishes between
// --apply's two busy checks relinks a root or a lockfile names a new entry,
// and either moves an mtime here; --apply re-reads the fence right before the
// first rename and refuses on any change.
type walkFence []pathStamp

type pathStamp struct {
	path string
	mod  time.Time
}

// stamp records path as it is right before the walk reads it.
func (f *walkFence) stamp(path string) {
	var mod time.Time
	if info, err := os.Lstat(path); err == nil {
		mod = info.ModTime()
	}
	*f = append(*f, pathStamp{path: path, mod: mod})
}

// moved returns the first stamped path that appeared, vanished or changed.
func (f walkFence) moved() (string, bool) {
	for _, s := range f {
		info, err := os.Lstat(s.path)
		if err != nil {
			if s.mod.IsZero() && errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return s.path, true
		}
		if s.mod.IsZero() || !info.ModTime().Equal(s.mod) {
			return s.path, true
		}
	}
	return "", false
}

// podfileBunEntry captures the .bun entry in a Podfile.lock `:path:`, e.g.
// "../../../node_modules/.bun/@expo+ui@57.0.8+8f55c570947bfdb5/node_modules/@expo/ui/ios".
var podfileBunEntry = regexp.MustCompile(`node_modules/\.bun/([^/"'\s]+)/`)

// nativeBunRefs reads each package directory's ios/Podfile.lock and returns
// the .bun entries it names, plus the lockfiles found (checkout-relative).
// Android keeps none: settings.gradle resolves modules at every configure.
func nativeBunRefs(checkout string, dirs []string, fence *walkFence) ([]string, []string, error) {
	var names []string
	manifests := []string{}
	for _, dir := range dirs {
		lock := filepath.Join(dir, "ios", "Podfile.lock")
		fence.stamp(lock)
		raw, err := os.ReadFile(lock)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		rel, _ := filepath.Rel(checkout, lock)
		manifests = append(manifests, rel)
		for _, m := range podfileBunEntry.FindAllSubmatch(raw, -1) {
			names = append(names, string(m[1]))
		}
	}
	return names, manifests, nil
}

// bunPackageDirs is the checkout plus every workspace package directory.
func bunPackageDirs(checkout string) ([]string, error) {
	dirs := []string{checkout}
	patterns, err := workspacePatterns(filepath.Join(checkout, "package.json"))
	if err != nil {
		return nil, err
	}
	for _, pattern := range patterns {
		if strings.HasPrefix(pattern, "!") {
			continue // an exclusion only shrinks the set; walking it anyway is the safe side
		}
		if strings.Contains(pattern, "**") {
			return nil, fmt.Errorf("workspace pattern %q: ** is not expanded here; refusing rather than missing a root", pattern)
		}
		matches, err := filepath.Glob(filepath.Join(checkout, filepath.FromSlash(pattern)))
		if err != nil {
			return nil, fmt.Errorf("workspace pattern %q: %w", pattern, err)
		}
		for _, m := range matches {
			// `apps/*` also matches files (apps/.DS_Store): only a directory
			// can be a workspace package.
			if info, err := os.Stat(m); err == nil && info.IsDir() {
				dirs = append(dirs, m)
			}
		}
	}
	return dirs, nil
}

// workspacePatterns reads package.json "workspaces": an array, or an object
// whose "packages" is one.
func workspacePatterns(manifest string) ([]string, error) {
	raw, err := os.ReadFile(manifest)
	if err != nil {
		return nil, err
	}
	var pkg struct {
		Workspaces json.RawMessage `json:"workspaces"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		return nil, fmt.Errorf("%s: %w", manifest, err)
	}
	if len(pkg.Workspaces) == 0 {
		return nil, nil
	}
	var list []string
	if err := json.Unmarshal(pkg.Workspaces, &list); err == nil {
		return list, nil
	}
	var object struct {
		Packages []string `json:"packages"`
	}
	if err := json.Unmarshal(pkg.Workspaces, &object); err != nil {
		return nil, fmt.Errorf("%s: workspaces: %w", manifest, err)
	}
	return object.Packages, nil
}

// scanModuleLinks hands every symlink in a node_modules directory to mark,
// one level into @scope directories and .bin, stamping each directory it
// reads. Real package directories are not links and are skipped.
func scanModuleLinks(dir string, mark func(string), fence *walkFence) error {
	fence.stamp(dir)
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(dir, name)
		switch {
		case entry.Type()&fs.ModeSymlink != 0:
			mark(path)
		case entry.IsDir() && (strings.HasPrefix(name, "@") || name == ".bin"):
			fence.stamp(path)
			sub, err := os.ReadDir(path)
			if err != nil {
				return err
			}
			for _, s := range sub {
				if s.Type()&fs.ModeSymlink != 0 {
					mark(filepath.Join(path, s.Name()))
				}
			}
		}
	}
	return nil
}

// bunStoreTarget resolves a symlink lexically against its own directory (a
// real directory in every caller) and returns the .bun entry it lands in.
func bunStoreTarget(stores []string, link string) (string, bool) {
	target, err := os.Readlink(link)
	if err != nil {
		return "", false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(link), target)
	}
	target = filepath.Clean(target)
	for _, store := range stores {
		rel, err := filepath.Rel(store, target)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		name, _, _ := strings.Cut(rel, string(filepath.Separator))
		if name == "node_modules" {
			return "", false // the fallback directory is a root of its own
		}
		return name, true
	}
	return "", false
}

// sizeDedup sums regular files under path, each hardlinked inode once across
// every call sharing seen.
func sizeDedup(ctx context.Context, path string, seen map[fileID]bool) (int64, error) {
	var total int64
	err := filepath.WalkDir(path, func(p string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return nil
		}
		if id, shared := hardlinkID(info); shared {
			if seen[id] {
				return nil
			}
			seen[id] = true
		}
		total += info.Size()
		return nil
	})
	return total, err
}

// refuseBusyCheckout compares case-folded: lsof reports a cwd in the disk's
// own case, while a checkout reached as ~/work/app on case-insensitive APFS
// keeps the typed case through Abs and EvalSymlinks. On a case-sensitive disk
// the fold can only refuse more. Only a writer counts: a bun whose command
// line is install-family (bunCheckoutWriterArgs), or one whose command line
// is unknown. Any other bun (a dev server, a script such as a session
// launcher) only resolves modules, and resolution from the checkout reaches
// only reachable entries, which a prune keeps. A cwd in a nested bun project
// (nestedBunProject) does not count either.
func refuseBusyCheckout(ctx context.Context, opts BunPruneOptions, spellings []string) error {
	procs, err := opts.BunCwds(ctx)
	if err != nil {
		return fmt.Errorf("cannot list running bun processes, refusing to delete: %w", err)
	}
	var inside []string
	for _, p := range procs {
		if p.Args != "" && !bunCheckoutWriterArgs.MatchString(p.Args) {
			continue
		}
		cwd := strings.ToLower(p.Cwd)
		for _, root := range spellings {
			root = strings.ToLower(root)
			if cwd == root || strings.HasPrefix(cwd, root+string(filepath.Separator)) {
				if !nestedBunProject(p.Cwd, root) {
					args := firstField(p.Args, 3)
					if args == "" {
						args = "command line unknown"
					}
					inside = append(inside, fmt.Sprintf("%d (%s, in %s)", p.PID, args, p.Cwd))
				}
				break
			}
		}
	}
	if len(inside) > 0 {
		return fmt.Errorf("bun is running inside %s: pid %s; rerun once it finishes", spellings[len(spellings)-1], strings.Join(inside, ", "))
	}
	return nil
}

// nestedBunProject reports whether cwd, under the checkout that folds to
// root, is inside a checkout nested in it (a directory with its own .git
// entry: a worktree at .worktrees/<slug>, a submodule) that bun treats as a
// project of its own. bun resolves `bun install` to the nearest package.json
// at or above the cwd, or to the workspace root that lists it, so that
// package.json must lie inside the nested checkout and must not be one of the
// checkout's workspace packages. Such a process writes only its own
// node_modules, and walking up into the checkout's node_modules resolves only
// through reachable entries, which a prune keeps. cwd is stat'ed as lsof
// spells it, the disk's own case.
func nestedBunProject(cwd, root string) bool {
	var below []string // cwd and its ancestors under the checkout, innermost first
	dir := cwd
	for strings.ToLower(dir) != root {
		below = append(below, dir)
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
	// dir is now the checkout as the disk spells it.
	pkg := ""
	for _, d := range below {
		if pkg == "" {
			if info, err := os.Stat(filepath.Join(d, "package.json")); err == nil && info.Mode().IsRegular() {
				pkg = d
			}
		}
		if pkg == "" {
			continue
		}
		if info, err := os.Lstat(filepath.Join(d, ".git")); err == nil && (info.IsDir() || info.Mode().IsRegular()) {
			workspaces, err := bunPackageDirs(dir)
			if err != nil {
				return false // unreadable workspaces: count it, the safe side
			}
			for _, w := range workspaces {
				if strings.EqualFold(w, pkg) {
					return false // bun installs a workspace package at the checkout root
				}
			}
			return true
		}
	}
	return false
}

// lsofBunCwds reads every bun process's working directory in one lsof call,
// then the command lines of the whole table in one ps call. lsof exits 1 when
// nothing matched, which is the common, quiet answer. A pid that exited
// between the two calls keeps an empty Args.
func lsofBunCwds(ctx context.Context) ([]BunProcess, error) {
	out, err := exec.CommandContext(ctx, "lsof", "-a", "-c", "bun", "-d", "cwd", "-Fpn").Output()
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 {
			return nil, fmt.Errorf("lsof: %w", err)
		}
	}
	procs := parseLsofCwds(out)
	if len(procs) == 0 {
		return nil, nil
	}
	table, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,args=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	args := map[int]string{}
	for _, row := range parsePs(table) {
		args[row.pid] = row.args
	}
	for i := range procs {
		procs[i].Args = args[procs[i].PID]
	}
	return procs, nil
}

// parseLsofCwds reads `lsof -Fpn` field output: a p<pid> line opens each
// process, an n<path> line names its cwd.
func parseLsofCwds(out []byte) []BunProcess {
	var procs []BunProcess
	pid := 0
	for _, line := range bytes.Split(out, []byte("\n")) {
		if len(line) < 2 {
			continue
		}
		switch line[0] {
		case 'p':
			pid, _ = strconv.Atoi(string(line[1:]))
		case 'n':
			if pid > 0 {
				procs = append(procs, BunProcess{PID: pid, Cwd: string(line[1:])})
			}
		}
	}
	return procs
}

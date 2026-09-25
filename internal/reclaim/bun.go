package reclaim

// The bun step. With `globalStore = true` (FixIt's bunfig) every checkout's
// node_modules/.bun/<pkg> is an absolute symlink into
// ~/.bun/install/cache/links/<pkg>-<hash>. Deleting the cache wholesale, which
// this step did until 2026-09-25, dangles every such checkout at once, and
// the repair is one `bun install` per checkout: the install storm a full disk
// can least afford. The step keeps links/ and deletes only what bun derives
// again on its own: extracted tarballs (<pkg>@<ver>@@@N), the per-name index
// directories of version symlinks beside them (also under @scope/), and the
// *.npm registry manifests. Anything else in the cache stays (bunCacheTargets).
//
// Measured before allowing that (read-only, 2026-09-25, docs/reclaim.md):
// every one of the 8,337 dependency symlinks inside links/ resolves to a
// links/ entry (7,619 to a sibling, 718 within its own entry, none outside),
// and sampled links/ files carry their own inode with a link count of 1 -
// APFS clones, not hardlinks or symlinks into the extracted directories. In a
// throwaway store, deleting the extracted directories left the checkout
// resolving, a frozen install reported no changes and re-extracted nothing,
// and a links/ entry deleted on purpose was downloaded again, not broken.
// The cost: the next install that materializes a NEW links/ entry, or a
// project-local package (patched and trusted packages are cloned into the
// checkout), downloads that tarball again.
//
// Two guards against an install running beside the step: it skips while any
// bun install-family process or bunx is alive, and every target is renamed into a
// trash directory beside the cache before it is deleted, so an install that
// starts mid-step sees a whole tarball or none, never a half-deleted one it
// would clone into a links/ entry that every checkout then shares.

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// bunLinksDir is the global virtual store inside the cache. Never a target.
const bunLinksDir = "links"

// bunTrashPrefix names the rename-then-delete staging directories, created
// beside the cache (same volume, so the rename is atomic).
const bunTrashPrefix = ".reclaim-trash-"

// bunInstallVerbs are the install family, aliases included (bun 1.3 help):
// the bun verbs that write a checkout's node_modules. init and create install
// the project they make.
var bunInstallVerbs = []string{"install", "i", "ci", "add", "a", "remove", "rm", "uninstall", "update", "upgrade", "link", "unlink", "pm", "patch", "patch-commit", "init", "create", "c"}

// bunVerbPattern is the one bun command-line parser: it matches a bun whose
// verb is one of verbs, after any global flags. bun takes the first word
// without a dash as its verb (`bun --cwd=x install` installs in x, while
// `bun --cwd x install` runs bunx on a package named install); the pattern
// also lets each flag take one value, so it matches both. It can over-count a
// writer, never miss one.
func bunVerbPattern(verbs ...string) string {
	return `(^|/)bun( +-\S+( +[^-\s]\S*)?)* +(` + strings.Join(verbs, "|") + `)( |$)`
}

var (
	// bunInstallArgs matches a process that writes the bun cache: the install
	// family, or bunx (bun x), which installs into the same cache.
	bunInstallArgs = regexp.MustCompile(bunVerbPattern(bunInstallVerbs...) + `|` + bunVerbPattern("x") + `|(^|/)bunx( |$)`)
	// bunCheckoutWriterArgs matches a process that writes a checkout's
	// node_modules: the install family. Not bunx, which installs into a temp
	// directory of its own, never into the checkout it runs in.
	bunCheckoutWriterArgs = regexp.MustCompile(bunVerbPattern(bunInstallVerbs...))
)

// psRow is one line of `ps -axo pid=,args=`.
type psRow struct {
	pid  int
	args string
}

// parsePs reads `ps -axo pid=,args=` output in table order.
func parsePs(out []byte) []psRow {
	var rows []psRow
	for _, line := range strings.Split(string(out), "\n") {
		pid, args, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(pid)
		if err != nil {
			continue
		}
		rows = append(rows, psRow{pid: n, args: strings.TrimSpace(args)})
	}
	return rows
}

func bunCacheDir(home string) string {
	return filepath.Join(home, ".bun", "install", "cache")
}

// bunCacheTargets lists what the step deletes, and only entries it recognizes
// as regenerable: an extracted tarball (a directory named with bun's `@@@`
// marker: <pkg>@<ver>@@@N, its _patch_hash= variants, @T@<hash> tarball-URL
// sources), a per-name index directory holding nothing but version symlinks,
// and a *.npm manifest file, at the root or one level into an @scope
// directory (the scope directory itself stays). Everything else stays:
// links/, dot-entries (bun's in-flight staging) and anything unrecognized,
// such as the 69 MB bun-darwin-x64-v1.3.14 executable this Mac's cache root
// held on 2026-09-25, a user's file, or a directory a future bun adds.
func bunCacheTargets(cache string) ([]string, error) {
	entries, err := os.ReadDir(cache)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(cache, name)
		switch {
		case name == bunLinksDir || strings.HasPrefix(name, "."):
		case strings.HasPrefix(name, "@") && !strings.Contains(name, "@@@") && entry.IsDir():
			scoped, err := os.ReadDir(path)
			if err != nil {
				return nil, err
			}
			for _, s := range scoped {
				if bunRegenerable(filepath.Join(path, s.Name()), s) {
					out = append(out, filepath.Join(path, s.Name()))
				}
			}
		case bunRegenerable(path, entry):
			out = append(out, path)
		}
	}
	return out, nil
}

// bunRegenerable recognizes an extracted tarball, a name index or a manifest.
func bunRegenerable(path string, entry fs.DirEntry) bool {
	name := entry.Name()
	switch {
	case entry.Type().IsRegular():
		return strings.HasSuffix(name, ".npm")
	case !entry.IsDir() || strings.HasPrefix(name, "."):
		return false
	case strings.Contains(name, "@@@"):
		return true
	}
	versions, err := os.ReadDir(path)
	if err != nil {
		return false
	}
	for _, v := range versions {
		if v.Type()&fs.ModeSymlink == 0 {
			return false
		}
	}
	return true
}

// bunInstallRunning reports the first live process that writes the cache.
// An unreadable process table counts as busy: the step cannot prove it safe.
func bunInstallRunning(ctx context.Context, env Env) (string, bool) {
	out, err := env.Exec(ctx, "ps", "-axo", "pid=,args=")
	if err != nil {
		return "the process table is unreadable", true
	}
	for _, row := range parsePs(out) {
		if bunInstallArgs.MatchString(row.args) {
			return "pid " + strconv.Itoa(row.pid) + ": " + firstField(row.args, 3), true
		}
	}
	return "", false
}

func firstField(s string, n int) string {
	f := strings.Fields(s)
	if len(f) > n {
		f = f[:n]
	}
	return strings.Join(f, " ")
}

func bunCacheStep(ctx context.Context, env Env) (int64, error) {
	if who, busy := bunInstallRunning(ctx, env); busy {
		return 0, skipError("a bun install is running (" + who + "); rerun with --only bun once it finishes")
	}
	cache := bunCacheDir(env.Home)
	targets, err := bunCacheTargets(cache)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	trash := filepath.Join(filepath.Dir(cache), bunTrashPrefix+strconv.Itoa(os.Getpid()))
	if err := os.MkdirAll(trash, 0o700); err != nil {
		return 0, err
	}
	var errs []error
	for i, target := range targets {
		if ctx.Err() != nil {
			break // what is not yet renamed stays whole in the cache
		}
		if err := os.Rename(target, filepath.Join(trash, strconv.Itoa(i))); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	// This run's trash plus any an interrupted run left behind.
	trashes, _ := filepath.Glob(filepath.Join(filepath.Dir(cache), bunTrashPrefix+"*"))
	var total int64
	for _, dir := range trashes {
		n, err := removeTree(ctx, dir, false)
		total += n
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return total, errors.Join(errs...)
}

func bunCacheSize(ctx context.Context, env Env) (int64, error) {
	targets, err := bunCacheTargets(bunCacheDir(env.Home))
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var total int64
	for _, target := range targets {
		n, err := treeSize(ctx, target)
		total += n
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return total, err
		}
	}
	return total, nil
}

package reclaim

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Leftovers in the per-user temp root (getconf DARWIN_USER_TEMP_DIR and its X/
// sibling): scratch a crashed or killed tool never cleaned up. Each step
// removes whole direct children by the entry's own mtime, never by the age of
// the files inside it.
const (
	instrumentsMinAge = 12 * time.Hour
	cloneMinAge       = time.Hour
	bunTmpMinAge      = 24 * time.Hour
)

// bunTmpRe is bun's install extraction dir: .<hex>-<HEX>.<package>.
var bunTmpRe = regexp.MustCompile(`^\.[0-9a-f]+-[0-9A-F]+\.`)

// tempStep builds a step's Run and Size from the list of paths it may remove.
func tempStep(list func(context.Context, Env) ([]string, error)) (run, size func(context.Context, Env) (int64, error)) {
	do := func(dry bool) func(context.Context, Env) (int64, error) {
		return func(ctx context.Context, env Env) (int64, error) {
			if env.TempDir == "" {
				return 0, skipError("no per-user temp dir")
			}
			paths, err := list(ctx, env)
			var skip skipError
			if errors.As(err, &skip) {
				return 0, err
			}
			var total int64
			errs := []error{err} // an incomplete listing still deletes what it proved stale
			for _, p := range paths {
				n, err := removeTree(ctx, p, dry)
				total += n
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					errs = append(errs, err)
				}
			}
			return total, errors.Join(errs...)
		}
	}
	return do(false), do(true)
}

// staleChildren lists dir's direct children whose name passes match and whose
// own mtime is at least minAge old (0 = any age). dir must be a real directory, never a
// symlink, and with dirsOnly so must every child returned — a symlinked level
// would carry the recursive delete outside the temp root. A missing dir is
// empty; every other read or stat error is returned with what was listed.
func staleChildren(env Env, dir string, minAge time.Duration, match func(string) bool, dirsOnly bool) ([]string, error) {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s: not a real directory", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	var errs []error
	for _, e := range entries {
		if !match(e.Name()) || (dirsOnly && !e.IsDir()) {
			continue
		}
		if minAge == 0 { // a container level: its mtime moves whenever a child goes
			out = append(out, filepath.Join(dir, e.Name()))
			continue
		}
		fi, err := e.Info()
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
			continue
		}
		if env.Now.Sub(fi.ModTime()) < minAge {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out, errors.Join(errs...)
}

// instrumentsTraces: raw recording scratch xctrace leaves when a recording is
// interrupted; saved .trace documents live elsewhere.
func instrumentsTraces(_ context.Context, env Env) ([]string, error) {
	return staleChildren(env, filepath.Clean(env.TempDir), instrumentsMinAge, func(name string) bool {
		return strings.HasPrefix(name, "instruments") && strings.HasSuffix(name, ".ktrace")
	}, false)
}

func bunInstallTemp(_ context.Context, env Env) ([]string, error) {
	return staleChildren(env, filepath.Clean(env.TempDir), bunTmpMinAge, bunTmpRe.MatchString, false)
}

// browserClones: Chromium browsers clone their app bundle into
// X/<bundle-id>.code_sign_clone/code_sign_clone.<rand>/<Name>.app.bundle at
// launch and delete it only on a clean quit. A running browser does not hold
// its clone open, so liveness is by time: a clone created after the oldest
// running process of that app started may belong to it and stays.
func browserClones(ctx context.Context, env Env) ([]string, error) {
	x := filepath.Join(filepath.Dir(filepath.Clean(env.TempDir)), "X")
	parents, err := staleChildren(env, x, 0, func(name string) bool { return strings.HasSuffix(name, ".code_sign_clone") }, true)
	errs := []error{err}
	var candidates []string
	for _, parent := range parents {
		clones, err := staleChildren(env, parent, cloneMinAge, func(name string) bool { return strings.HasPrefix(name, "code_sign_clone.") }, true)
		candidates = append(candidates, clones...)
		errs = append(errs, err)
	}
	if len(candidates) == 0 {
		return nil, errors.Join(errs...)
	}
	out, err := env.Exec(ctx, "ps", "-axo", "etime=,command=")
	if err != nil {
		return nil, skipError("ps failed; cannot prove a clone is unused")
	}
	started := oldestStarts(string(out), env.Now)
	var free []string
	for _, c := range candidates {
		bundles, _ := filepath.Glob(filepath.Join(c, "*.app.bundle"))
		info, err := os.Lstat(c)
		if err != nil || len(bundles) != 1 {
			continue // unrecognized layout: not ours to judge
		}
		app := strings.TrimSuffix(filepath.Base(bundles[0]), ".app.bundle")
		if start, running := started[app]; running && !info.ModTime().Before(start.Add(-time.Minute)) {
			continue
		}
		free = append(free, c)
	}
	return free, errors.Join(errs...)
}

// oldestStarts maps an app name to the start time of its oldest running main
// process, from `ps -axo etime=,command=` lines ("<etime> /…/<Name>.app/Contents/MacOS/…").
func oldestStarts(psOut string, now time.Time) map[string]time.Time {
	starts := map[string]time.Time{}
	for _, line := range strings.Split(psOut, "\n") {
		etime, cmd, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok {
			continue
		}
		before, _, ok := strings.Cut(cmd, ".app/Contents/MacOS/")
		if !ok {
			continue
		}
		app := before[strings.LastIndex(before, "/")+1:]
		age, ok := parseEtime(etime)
		if !ok {
			continue
		}
		start := now.Add(-age)
		if prev, seen := starts[app]; !seen || start.Before(prev) {
			starts[app] = start
		}
	}
	return starts
}

// parseEtime reads ps's elapsed time: [[dd-]hh:]mm:ss.
func parseEtime(s string) (time.Duration, bool) {
	var days int
	if d, rest, ok := strings.Cut(s, "-"); ok {
		n, err := strconv.Atoi(d)
		if err != nil {
			return 0, false
		}
		days, s = n, rest
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	var secs int
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return 0, false
		}
		secs = secs*60 + n
	}
	return time.Duration(days)*24*time.Hour + time.Duration(secs)*time.Second, true
}

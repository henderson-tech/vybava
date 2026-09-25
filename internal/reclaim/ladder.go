package reclaim

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Ladder is the frozen step list. Order within a tier is expected yield on a
// working dev Mac (measured: go-build 77G, bun 17G, DeviceSupport 17G, npm
// 8G, gradle 5G, Docker build cache 24G — all invisible to a named-bucket
// cache list until someone looked). Adding a step is one entry here plus a
// line in docs/reclaim.md; nothing else knows the list.
func Ladder(env Env) []Step {
	mac := env.GOOS == "darwin"
	steps := []Step{
		// ---- tier 1: seconds, huge ------------------------------------
		{ID: "go-build", Tier: TierBulk, Title: "Go build cache", Regenerates: "next go build",
			Paths: []string{"~/Library/Caches/go-build", "~/.cache/go-build"}},
		{ID: "docker-builder", Tier: TierBulk, Title: "Docker build cache", Regenerates: "next docker build", Needs: "docker",
			Run: dockerPrune("builder", "prune", "-af"), Size: dockerDF("BuildCache")},
		{ID: "docker-images", Tier: TierBulk, Title: "Docker unreferenced images", Regenerates: "re-pull / rebuild", Needs: "docker",
			Run: dockerPrune("image", "prune", "-af"), Size: dockerDF("Images")},
		{ID: "bun", Tier: TierBulk, Title: "bun cache tarballs + manifests (keeps links/)", Regenerates: "next install re-downloads what it newly materializes; links/ checkouts keep working",
			Run: bunCacheStep, Size: bunCacheSize},
		{ID: "npm", Tier: TierBulk, Title: "npm cache + npx store", Regenerates: "next npm install / npx",
			Paths: []string{"~/.npm/_cacache", "~/.npm/_npx"}},
		{ID: "derived-data", Tier: TierBulk, Title: "Xcode DerivedData", Regenerates: "next Xcode build",
			Paths: []string{"~/Library/Developer/Xcode/DerivedData/*"}},
		{ID: "gradle", Tier: TierBulk, Title: "Gradle caches", Regenerates: "next gradle build",
			Paths: []string{"~/.gradle/caches"}},
		{ID: "pnpm", Tier: TierBulk, Title: "pnpm cache + unreferenced store", Regenerates: "next pnpm install", Needs: "pnpm",
			Run: func(ctx context.Context, env Env) (int64, error) {
				n, err := removeTree(ctx, filepath.Join(env.Home, "Library/Caches/pnpm"), false)
				if err != nil && !os.IsNotExist(err) {
					return n, err
				}
				_, err = env.Exec(ctx, "pnpm", "store", "prune")
				return n, err
			}},
		{ID: "cargo", Tier: TierBulk, Title: "cargo registry cache", Regenerates: "next cargo build",
			Paths: []string{"~/.cargo/registry/cache", "~/.cargo/git/checkouts"}},
		{ID: "py", Tier: TierBulk, Title: "pip / uv caches", Regenerates: "next pip / uv install",
			Paths: []string{"~/Library/Caches/pip", "~/.cache/pip", "~/.cache/uv", "~/Library/Caches/uv"}},

		// ---- tier 2: tool & app caches ----------------------------------
		{ID: "tool-caches", Tier: TierCaches, Title: "tool caches (playwright-mcp, CocoaPods, dotslash, claude, codex, copilot, composer, yarn, turbo, nx)", Regenerates: "next use",
			Paths: []string{
				"~/Library/Caches/ms-playwright-mcp", "~/Library/Caches/CocoaPods", "~/Library/Caches/dotslash",
				"~/Library/Caches/claude-cli-nodejs", "~/Library/Caches/com.openai.codex", "~/Library/Caches/copilot",
				"~/Library/Caches/composer", "~/Library/Caches/Yarn", "~/.cache/yarn", "~/.cache/turbo", "~/.cache/nx",
				"~/Library/Caches/com.todesktop.230313mzl4w4u92.ShipIt", "~/Library/Caches/JetBrains",
			}},
		{ID: "browser-caches", Tier: TierCaches, Title: "browser & app caches (Brave, Chrome, Spotify)", Regenerates: "next launch",
			Paths: []string{"~/Library/Caches/BraveSoftware", "~/Library/Caches/Google", "~/Library/Caches/com.spotify.client"}},
		{ID: "playwright", Tier: TierCaches, Title: "orphaned Playwright browser revisions", Regenerates: "nothing — only unpinned revisions go", Needs: "pwmcp",
			Run: func(ctx context.Context, env Env) (int64, error) {
				_, err := env.Exec(ctx, "pwmcp", "prune")
				return 0, err
			}},
		{ID: "brew", Tier: TierCaches, Title: "Homebrew downloads + old versions", Regenerates: "next brew install", Needs: "brew",
			Run: func(ctx context.Context, env Env) (int64, error) {
				_, err := env.Exec(ctx, "brew", "cleanup", "-s")
				n, _ := removeTree(ctx, filepath.Join(env.Home, "Library/Caches/Homebrew"), false)
				return n, err
			}},
		{ID: "maven", Tier: TierCaches, Title: "Maven local repository", Regenerates: "next mvn build",
			Paths: []string{"~/.m2/repository"}},
		{ID: "logs", Tier: TierCaches, Title: "rotated app logs (JetBrains, CreativeCloud, *.log.old.*)", Regenerates: "nothing",
			Paths: []string{"~/Library/Logs/JetBrains", "~/Library/Logs/CreativeCloud", "~/Library/Logs/*.log.old.*"}},
		{ID: "xcode-caches", Tier: TierCaches, Title: "Xcode + CoreSimulator caches", Regenerates: "next Xcode run",
			Paths: []string{"~/Library/Caches/com.apple.dt.Xcode", "~/Library/Developer/CoreSimulator/Caches"}},
		{ID: "sim-unavailable", Tier: TierCaches, Title: "simulators whose runtime is gone", Regenerates: "nothing", Needs: "xcrun",
			Run: func(ctx context.Context, env Env) (int64, error) {
				_, err := env.Exec(ctx, "xcrun", "simctl", "delete", "unavailable")
				return 0, err
			}},

		// ---- tier 3: aggressive, still reversible -----------------------
		{ID: "device-support", Tier: TierAggressive, Title: "iOS DeviceSupport symbols", Regenerates: "re-syncs from the next plugged-in device",
			Paths: []string{"~/Library/Developer/Xcode/iOS DeviceSupport/*", "~/Library/Developer/Xcode/watchOS DeviceSupport/*", "~/Library/Developer/Xcode/tvOS DeviceSupport/*"}},
		{ID: "sim-runtimes", Tier: TierAggressive, Title: "simulator runtimes with no device", Regenerates: "re-download via Xcode", Needs: "xcrun",
			Run: deleteUnusedRuntimes, Size: sizeUnusedRuntimes},
		{ID: "sim-logs", Tier: TierAggressive, Title: "simulator diagnostics logs (shuts sims down; apps + data survive)", Regenerates: "nothing", Needs: "xcrun",
			Run: func(ctx context.Context, env Env) (int64, error) {
				_, _ = env.Exec(ctx, "xcrun", "simctl", "shutdown", "all")
				var total int64
				matches, _ := filepath.Glob(filepath.Join(env.Home, "Library/Developer/CoreSimulator/Devices/*/data/var/db/diagnostics"))
				for _, m := range matches {
					n, _ := removeTree(ctx, m, false)
					total += n
				}
				return total, nil
			},
			Size: func(ctx context.Context, env Env) (int64, error) {
				var total int64
				matches, _ := filepath.Glob(filepath.Join(env.Home, "Library/Developer/CoreSimulator/Devices/*/data/var/db/diagnostics"))
				for _, m := range matches {
					n, _ := treeSize(ctx, m)
					total += n
				}
				return total, nil
			}},
		{ID: "messages-tmp", Tier: TierAggressive, Title: "Messages sandbox temp, files older than keep-days (quits Messages)", Regenerates: "re-fetched from iCloud on view",
			Aged: true, Quit: "Messages", Paths: []string{"~/Library/Containers/com.apple.MobileSMS/Data/tmp"}},
		{ID: "sandbox-tmp", Tier: TierAggressive, Title: "other app sandbox temp, files older than keep-days", Regenerates: "app re-creates",
			Aged: true, Except: []string{"com.apple.MobileSMS"}, Paths: []string{"~/Library/Containers/*/Data/tmp"}},
		{ID: "trash", Tier: TierAggressive, Title: "Trash", Regenerates: "nothing — already thrown away",
			Paths: []string{"~/.Trash/*"}},
	}
	if !mac {
		var out []Step
		for _, s := range steps {
			if strings.Contains(s.ID, "sim") || s.ID == "device-support" || s.ID == "xcode-caches" || s.ID == "derived-data" || s.ID == "messages-tmp" || s.ID == "sandbox-tmp" || s.ID == "trash" || s.ID == "brew" {
				continue
			}
			out = append(out, s)
		}
		return out
	}
	return steps
}

var reclaimedRe = regexp.MustCompile(`(?i)total reclaimed space:\s*([\d.]+)\s*([KMGT]?B)`)

func dockerPrune(args ...string) func(context.Context, Env) (int64, error) {
	return func(ctx context.Context, env Env) (int64, error) {
		out, err := env.Exec(ctx, "docker", args...)
		if err != nil {
			return 0, err
		}
		return ParseReclaimed(string(out)), nil
	}
}

// ParseReclaimed reads Docker's "Total reclaimed space: 24.4GB" line.
func ParseReclaimed(out string) int64 {
	m := reclaimedRe.FindStringSubmatch(out)
	if m == nil {
		return 0
	}
	return parseSize(m[1], m[2])
}

func parseSize(num, unit string) int64 {
	f, err := strconv.ParseFloat(num, 64)
	if err != nil {
		return 0
	}
	mult := map[string]float64{"B": 1, "KB": 1e3, "MB": 1e6, "GB": 1e9, "TB": 1e12}[strings.ToUpper(unit)]
	if mult == 0 {
		mult = 1
	}
	return int64(f * mult)
}

var dfRe = regexp.MustCompile(`([\d.]+)([KMGT]?B)\s*\(\d+%\)`)

// dockerDF sizes a `docker system df` row's reclaimable column for dry runs.
func dockerDF(row string) func(context.Context, Env) (int64, error) {
	return func(ctx context.Context, env Env) (int64, error) {
		out, err := env.Exec(ctx, "docker", "system", "df")
		if err != nil {
			return 0, err
		}
		for _, line := range strings.Split(string(out), "\n") {
			if !strings.HasPrefix(strings.ReplaceAll(line, " ", ""), row) {
				continue
			}
			if m := dfRe.FindStringSubmatch(line); m != nil {
				return parseSize(m[1], m[2]), nil
			}
		}
		return 0, nil
	}
}

// simRuntime is the slice of `simctl runtime list -j` we need.
type simRuntime struct {
	Identifier string `json:"identifier"`
	Version    string `json:"version"`
	Platform   string `json:"platformIdentifier"`
	SizeBytes  int64  `json:"sizeBytes"`
	Build      string `json:"build"`
}

// UnusedRuntimes returns runtimes that no simulator device uses, from the
// two simctl JSON listings. Exported for the test fixtures.
func UnusedRuntimes(runtimeListJSON, deviceListJSON []byte) ([]simRuntime, error) {
	var runtimes map[string]simRuntime
	if err := json.Unmarshal(runtimeListJSON, &runtimes); err != nil {
		return nil, fmt.Errorf("runtime list: %w", err)
	}
	var devices struct {
		Devices map[string][]struct {
			IsAvailable bool `json:"isAvailable"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(deviceListJSON, &devices); err != nil {
		return nil, fmt.Errorf("device list: %w", err)
	}
	used := map[string]bool{}
	for runtimeID, list := range devices.Devices {
		if len(list) > 0 {
			used[runtimeID] = true
		}
	}
	var unused []simRuntime
	for _, rt := range runtimes {
		key := runtimeKey(rt)
		if used[key] {
			continue
		}
		unused = append(unused, rt)
	}
	return unused, nil
}

// runtimeKey is how `simctl list devices -j` names a runtime:
// com.apple.CoreSimulator.SimRuntime.iOS-18-2.
func runtimeKey(rt simRuntime) string {
	return "com.apple.CoreSimulator.SimRuntime." + strings.ReplaceAll(rt.Platform, " ", "") + "-" + strings.ReplaceAll(rt.Version, ".", "-")
}

func listUnusedRuntimes(ctx context.Context, env Env) ([]simRuntime, error) {
	rts, err := env.Exec(ctx, "xcrun", "simctl", "runtime", "list", "-j")
	if err != nil {
		return nil, err
	}
	devs, err := env.Exec(ctx, "xcrun", "simctl", "list", "devices", "-j")
	if err != nil {
		return nil, err
	}
	return UnusedRuntimes(rts, devs)
}

func deleteUnusedRuntimes(ctx context.Context, env Env) (int64, error) {
	unused, err := listUnusedRuntimes(ctx, env)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, rt := range unused {
		if _, err := env.Exec(ctx, "xcrun", "simctl", "runtime", "delete", rt.Identifier); err != nil {
			return total, fmt.Errorf("runtime %s %s: %w", rt.Platform, rt.Version, err)
		}
		total += rt.SizeBytes
	}
	return total, nil
}

func sizeUnusedRuntimes(ctx context.Context, env Env) (int64, error) {
	unused, err := listUnusedRuntimes(ctx, env)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, rt := range unused {
		total += rt.SizeBytes
	}
	return total, nil
}

// Notes surfaces the by-hand items the ladder refuses to touch, using only
// cheap probes (a directory listing, one docker call) — never a du over a
// user tree.
func Notes(ctx context.Context, env Env) []Note {
	var notes []Note
	if env.GOOS == "darwin" {
		dir := filepath.Join(env.Home, "Library/Group Containers/group.com.apple.screencapture/ScreenRecordings")
		if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
			var total int64
			var files []string
			for _, e := range entries {
				if info, err := e.Info(); err == nil && !e.IsDir() {
					total += info.Size()
					files = append(files, fmt.Sprintf("%s (%s)", e.Name(), Human(info.Size())))
				}
			}
			if total > 0 {
				notes = append(notes, Note{Title: "unsaved screen recordings", Bytes: total,
					Detail: strings.Join(files, ", "), Action: fmt.Sprintf("open %q  # play, then trash by hand — user data", dir)})
			}
		}
	}
	if _, err := env.LookPath("docker"); err == nil {
		if out, err := env.Exec(ctx, "docker", "system", "df"); err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				if strings.HasPrefix(line, "Local Volumes") {
					if m := dfRe.FindStringSubmatch(line); m != nil {
						if n := parseSize(m[1], m[2]); n > 0 {
							notes = append(notes, Note{Title: "dangling Docker volumes", Bytes: n,
								Detail: "may be a paused worktree's database — classify by compose project name before deleting",
								Action: "bash ~/.claude/skills/reclaiming-disk-space/audit.sh  # ORPHAN vs MOVED verdicts, delete by name"})
						}
					}
				}
			}
		}
	}
	return notes
}

// Signed formats a delta with its sign.
func Signed(n int64) string {
	if n < 0 {
		return "-" + Human(-n)
	}
	return "+" + Human(n)
}

// Human formats bytes the way df does.
func Human(n int64) string {
	if n < 0 {
		n = -n
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%dB", n)
	}
	f := float64(n)
	for _, u := range []string{"K", "M", "G", "T"} {
		f /= unit
		if f < unit {
			if f >= 100 {
				return fmt.Sprintf("%.0f%s", f, u)
			}
			return fmt.Sprintf("%.1f%s", f, u)
		}
	}
	return fmt.Sprintf("%.1fP", f/unit)
}

// ParseHuman reads "100G", "1.5T", "512M" for --until.
func ParseHuman(s string) (int64, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	s = strings.TrimSuffix(s, "B")
	if s == "" {
		return 0, fmt.Errorf("empty size")
	}
	mult := int64(1)
	switch s[len(s)-1] {
	case 'K':
		mult = 1 << 10
	case 'M':
		mult = 1 << 20
	case 'G':
		mult = 1 << 30
	case 'T':
		mult = 1 << 40
	}
	if mult > 1 {
		s = s[:len(s)-1]
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, fmt.Errorf("size %q: %w", s, err)
	}
	return int64(f * float64(mult)), nil
}

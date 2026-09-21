package claudeguards

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/henderson-tech/vybava/internal/vconfig"
)

// Config is the guards section. A nil command list selects built-in commands;
// an explicit empty list disables only the unbounded-output rule.
type Config struct {
	NoRead            []string `json:"noRead,omitempty"`
	MaxDumpLines      int      `json:"maxDumpLines,omitempty"`
	UnboundedCommands []string `json:"unboundedCommands,omitempty"`
	AppiumSessionDirs []string `json:"appiumSessionDirs,omitempty"`
	TestWorkerCap     int      `json:"testWorkerCap,omitempty"`
	// DevboxOnly lists RE2 patterns; a local command segment matching one
	// must run through `devbox run` (machine:devbox-only).
	DevboxOnly []string `json:"devboxOnly,omitempty"`
	// SimCap is the most booted simulators this Mac may hold before a boot
	// is refused (machine:sim-cap); DevServerCap the same for Metro/next/
	// API dev servers (machine:dev-server-cap).
	SimCap       int `json:"simCap,omitempty"`
	DevServerCap int `json:"devServerCap,omitempty"`
	root         string
}

// defaultGuardConfig is what every rule reads when the repo sets nothing, or
// when its guards section fails to load.
func defaultGuardConfig() Config {
	return Config{MaxDumpLines: maxDumpLines, TestWorkerCap: defaultTestWorkerCap, SimCap: defaultSimCap, DevServerCap: defaultDevServerCap}
}

func loadGuardConfig(cwd string) (Config, error) {
	result := defaultGuardConfig()
	cfg, err := vconfig.Load(cwd)
	if errors.Is(err, vconfig.ErrNotFound) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	if err = cfg.Section("guards", &result); errors.Is(err, vconfig.ErrNoSection) {
		return result, nil
	} else if err != nil {
		return defaultGuardConfig(), err
	}
	result.root = cfg.Root
	if result.MaxDumpLines <= 0 {
		return defaultGuardConfig(), errors.New("guards.maxDumpLines must be positive")
	}
	if result.TestWorkerCap < 1 {
		return defaultGuardConfig(), errors.New("guards.testWorkerCap must be at least 1")
	}
	if result.SimCap < 1 {
		return defaultGuardConfig(), errors.New("guards.simCap must be at least 1")
	}
	if result.DevServerCap < 1 {
		return defaultGuardConfig(), errors.New("guards.devServerCap must be at least 1")
	}
	for _, pattern := range result.DevboxOnly {
		if _, err := regexp.Compile(pattern); err != nil {
			return defaultGuardConfig(), fmt.Errorf("guards.devboxOnly %q: %w", pattern, err)
		}
	}
	for name, patterns := range map[string][]string{"noRead": result.NoRead, "appiumSessionDirs": result.AppiumSessionDirs} {
		for _, pattern := range patterns {
			for _, part := range strings.Split(pattern, "/") {
				if _, err := path.Match(part, ""); err != nil {
					return defaultGuardConfig(), fmt.Errorf("guards.%s %q: %w", name, pattern, err)
				}
			}
		}
	}
	return result, nil
}

func guardConfig(cwd string) Config {
	cfg, err := loadGuardConfig(cwd)
	if err != nil {
		fmt.Fprintf(os.Stderr, "claude-guards: guards config unavailable; using defaults: %v\n", err)
	}
	return cfg
}

// MatchNoRead matches slash-separated repository paths; ** spans zero or more
// complete path components. Ordinary components use Go's glob syntax.
func MatchNoRead(pattern, name string) bool {
	parts, names := strings.Split(pattern, "/"), strings.Split(name, "/")
	var match func(int, int) bool
	match = func(i, j int) bool {
		if i == len(parts) {
			return j == len(names)
		}
		if parts[i] == "**" {
			return match(i+1, j) || (j < len(names) && match(i, j+1))
		}
		if j == len(names) {
			return false
		}
		ok, _ := path.Match(parts[i], names[j])
		return ok && match(i+1, j+1)
	}
	return match(0, 0)
}

func (cfg Config) noRead(abs string) bool {
	if cfg.root == "" {
		return false
	}
	rel, err := filepath.Rel(cfg.root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	for _, pattern := range cfg.NoRead {
		if MatchNoRead(pattern, filepath.ToSlash(rel)) {
			return true
		}
	}
	return false
}

func noReadDenial(file string) *Denial {
	return deny("context:no-read", file+": generated megafile — grep it, or read the source that generates it.", contextReadEscape)
}

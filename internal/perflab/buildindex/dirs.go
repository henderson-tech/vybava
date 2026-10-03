package buildindex

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Dirs are perflab's two roots: the cache (rebuildable GBs: builds,
// bundles, variants) and the state dir (locks). Env overrides
// PERFLAB_CACHE_DIR / PERFLAB_STATE_DIR exist for tests and sandboxes.
type Dirs struct {
	Cache string
	State string
}

// DefaultDirs resolves os.UserCacheDir()/perflab and
// ~/.local/state/perflab, honouring the env overrides.
func DefaultDirs() (Dirs, error) {
	cache := os.Getenv("PERFLAB_CACHE_DIR")
	if cache == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return Dirs{}, fmt.Errorf("resolve the user cache dir: %w", err)
		}
		cache = filepath.Join(base, "perflab")
	}
	state := os.Getenv("PERFLAB_STATE_DIR")
	if state == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Dirs{}, fmt.Errorf("resolve the home dir: %w", err)
		}
		state = filepath.Join(home, ".local", "state", "perflab")
	}
	return Dirs{Cache: cache, State: state}, nil
}

func (d Dirs) buildsDir() string   { return filepath.Join(d.Cache, "builds") }
func (d Dirs) bundlesDir() string  { return filepath.Join(d.Cache, "bundles") }
func (d Dirs) variantsDir() string { return filepath.Join(d.Cache, "variants") }
func (d Dirs) locksDir() string    { return filepath.Join(d.State, "locks") }

// errLockTimeout is what lockFile returns when the wait passed.
var errLockTimeout = errors.New("lock wait timed out")

// lockPoll is the retry interval of the non-blocking acquire loop.
const lockPoll = 50 * time.Millisecond

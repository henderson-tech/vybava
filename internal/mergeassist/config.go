// Package mergeassist takes the mechanical conflict classes out of a merge —
// locale catalogs (lok's driver), generated files, migration timestamps — so
// a session only reads genuine code conflicts. Configured through the `merge`
// section of vybava.config.ts; catalogs come from the `lok` section.
package mergeassist

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/henderson-tech/vybava/internal/lok"
	"github.com/henderson-tech/vybava/internal/vconfig"
)

// Closed diagnostic codes. Each fires for one reason and names its fix.
const (
	// DiagConfigMissing — no vybava.config.* above cwd.
	DiagConfigMissing = "CONFIG_MISSING"
	// DiagConfigInvalid — the merge section fails validation.
	DiagConfigInvalid = "CONFIG_INVALID"
	// DiagSetupDrift — info/attributes or the git driver registration is stale.
	DiagSetupDrift = "SETUP_DRIFT"
	// DiagDirty — the merge needs a clean tracked tree.
	DiagDirty = "DIRTY_TREE"
	// DiagMergeInProgress — a merge is already running (or none is, for status).
	DiagMergeInProgress = "MERGE_IN_PROGRESS"
	// DiagNoMerge — status/regen outside a merge with nothing journaled.
	DiagNoMerge = "NO_MERGE"
	// DiagOpen — conflicts remain for the session to resolve.
	DiagOpen = "CONFLICTS_OPEN"
	// DiagRegenFailed — a regen command exited non-zero.
	DiagRegenFailed = "REGEN_FAILED"
	// DiagMigrationRefused — a migration cannot be renumbered safely.
	DiagMigrationRefused = "MIGRATION_REFUSED"
	// DiagCheckFailed — the repo's own migration guard failed after a renumber.
	DiagCheckFailed = "CHECK_FAILED"
)

// Diag is one diagnostic; Fix is the exact next command when one exists.
type Diag struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

func (d *Diag) Error() string { return d.Code + ": " + d.Detail }

// Generated is one group of generated paths and the command rebuilding them.
type Generated struct {
	Paths []string `json:"paths"`
	Regen string   `json:"regen"`
}

// Migrations is one migration directory whose unmerged files get renumbered
// past the merged base's newest.
type Migrations struct {
	Dir   string `json:"dir"`
	Style string `json:"style"`
	// Step rounds the new timestamps: the first free multiple of Step above
	// the base's newest, then +1, +2, … (default 1e8).
	Step  int64  `json:"step,omitempty"`
	Check string `json:"check,omitempty"`
}

// StyleTypeORM — `<ts>-<Name>.ts` holding `class <Name><ts>` (and usually
// `name = '<Name><ts>'`); TypeORM orders by that trailing timestamp.
const StyleTypeORM = "typeorm"

const defaultStep = 100_000_000

// Config is the `merge` section.
type Config struct {
	Generated  []Generated  `json:"generated,omitempty"`
	Migrations []Migrations `json:"migrations,omitempty"`
}

// Validate checks the shape once so every verb can trust it.
func (c *Config) Validate() error {
	for i, g := range c.Generated {
		if len(g.Paths) == 0 || strings.TrimSpace(g.Regen) == "" {
			return fmt.Errorf("merge.generated[%d]: paths and regen are required", i)
		}
	}
	dirs := map[string]bool{}
	for i, m := range c.Migrations {
		if dirs[m.Dir] {
			return fmt.Errorf("merge.migrations[%d]: %s is listed twice; one entry (and one check) per directory", i, m.Dir)
		}
		dirs[m.Dir] = true
		if m.Dir == "" || m.Style != StyleTypeORM {
			return fmt.Errorf("merge.migrations[%d]: dir is required and style must be %q", i, StyleTypeORM)
		}
		if m.Step < 0 {
			return fmt.Errorf("merge.migrations[%d]: step must be positive", i)
		}
	}
	return nil
}

// Tool is one repository's merge configuration.
type Tool struct {
	Root   string
	Config Config
	// Lok is nil when the repo declares no catalogs.
	Lok *lok.Tool
}

// Open loads the merge (and lok) sections for cwd. A repo with neither is
// still usable: every conflict is then simply code.
func Open(cwd string) (*Tool, error) {
	cfg, err := vconfig.Load(cwd)
	if err != nil {
		if errors.Is(err, vconfig.ErrNotFound) {
			return nil, &Diag{Code: DiagConfigMissing, Detail: err.Error(), Fix: "vybava config init"}
		}
		return nil, &Diag{Code: DiagConfigInvalid, Detail: err.Error()}
	}
	t := &Tool{Root: cfg.Root}
	if err := cfg.Section("merge", &t.Config); err != nil && !errors.Is(err, vconfig.ErrNoSection) {
		return nil, &Diag{Code: DiagConfigInvalid, Detail: err.Error()}
	}
	if err := t.Config.Validate(); err != nil {
		return nil, &Diag{Code: DiagConfigInvalid, Detail: err.Error()}
	}
	lt, err := lok.Open(cfg.Root)
	var d *lok.Diag
	switch {
	case err == nil:
		t.Lok = lt
	case errors.As(err, &d) && d.Code == lok.DiagConfigMissing:
	default:
		return nil, &Diag{Code: DiagConfigInvalid, Detail: err.Error()}
	}
	return t, nil
}

// GeneratedFor returns the group owning a repository-relative path.
func (t *Tool) GeneratedFor(rel string) (Generated, bool) {
	for _, g := range t.Config.Generated {
		for _, p := range g.Paths {
			if vconfig.MatchPath(p, rel) {
				return g, true
			}
		}
	}
	return Generated{}, false
}

// git runs one git command at the repo root; stderr rides in the error.
func (t *Tool) git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = t.Root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

package installer

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// engineTypes is the folder Claude Code writes beside a mod at every load
// (the API's declarations and a tsconfig). It is the engine's: never
// shipped from the payload, and carried across an upgrade, never deleted.
const engineTypes = ".claude-plugin/types"

// engineTSConfig is the root tsconfig.json the engine writes beside a mod it
// loads from a folder (extending engineTypes); it is never shipped either.
const engineTSConfig = "tsconfig.json"

// installMod swaps a mod payload into a Claude skills folder, where every
// live session hot-reloads it on change. The payload is staged outside that
// folder and moved in by rename, and the prior copy is moved out before it
// is deleted: a session never sees a half-written module, a staging
// directory, or two plugins of one name.
func (i Installer) installMod(itemID, destination string) error {
	if err := ensureReplaceable(destination, itemID); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return fmt.Errorf("create skill home: %w", err)
	}
	root, err := i.stageRoot()
	if err != nil {
		return err
	}
	err = i.swapMod(root, itemID, destination)
	if errors.Is(err, syscall.EXDEV) {
		// The stage root is on another volume than the destination, so a
		// rename cannot cross; stage beside the skills folder instead.
		err = i.swapMod(fallbackStageRoot(destination), itemID, destination)
	}
	return err
}

// removeMod moves a managed mod out of the skills folder in one rename and
// deletes it there, so the engine sees it vanish whole.
func (i Installer) removeMod(itemID, destination string) error {
	if _, err := os.Stat(destination); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := ensureReplaceable(destination, itemID); err != nil {
		return err
	}
	root, err := i.stageRoot()
	if err != nil {
		return err
	}
	aside, err := moveAside(root, itemID, destination)
	if errors.Is(err, syscall.EXDEV) {
		aside, err = moveAside(fallbackStageRoot(destination), itemID, destination)
	}
	if err != nil {
		return err
	}
	return os.RemoveAll(aside)
}

func (i Installer) swapMod(root, itemID, destination string) error {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("create stage directory: %w", err)
	}
	staging, err := os.MkdirTemp(root, itemID+"-*")
	if err != nil {
		return fmt.Errorf("create staging directory: %w", err)
	}
	// keep is set when staging (the prior copy, after the exchange) must
	// outlive this call.
	keep := false
	defer func() {
		if !keep {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := i.stageModPayload(itemID, staging); err != nil {
		return err
	}

	if _, err := os.Lstat(destination); errors.Is(err, os.ErrNotExist) {
		if err := os.Rename(staging, destination); err != nil {
			return fmt.Errorf("activate mod: %w", err)
		}
		return nil
	} else if err != nil {
		return err
	}

	// One atomic exchange: the folder changes content and never vanishes.
	// staging then holds the prior copy, which the deferred RemoveAll deletes.
	err = exchange(staging, destination)
	if err == nil {
		if err := carryEngineTypes(staging, destination); err != nil {
			keep = true
			return keepPrior(staging, err)
		}
		return nil
	}
	if !exchangeUnsupported(err) {
		return fmt.Errorf("swap mod: %w", err)
	}

	// No exchange on this filesystem: two renames, the prior copy restored
	// if the second one fails and kept (never deleted) if that fails too.
	aside := staging + ".old"
	if err := os.Rename(destination, aside); err != nil {
		return fmt.Errorf("move prior mod aside: %w", err)
	}
	if err := os.Rename(staging, destination); err != nil {
		if restoreErr := os.Rename(aside, destination); restoreErr != nil {
			return errors.Join(fmt.Errorf("activate mod: %w", err), fmt.Errorf("restore prior mod, kept at %s: %w", aside, restoreErr))
		}
		return fmt.Errorf("activate mod: %w", err)
	}
	if err := carryEngineTypes(aside, destination); err != nil {
		return keepPrior(aside, err)
	}
	if err := os.RemoveAll(aside); err != nil {
		return fmt.Errorf("the new mod is active; remove the prior copy at %s: %w", aside, err)
	}
	return nil
}

// keepPrior is a failed carry's path: the new mod is already active, and the
// prior copy (its engine types included) stays where it is — its caller
// skips the cleanup — and is named instead of deleted.
func keepPrior(prior string, carryErr error) error {
	return fmt.Errorf("the new mod is active, but %w; the prior copy is kept at %s (the engine re-lays the types at its next load)", carryErr, prior)
}

func (i Installer) stageModPayload(itemID, staging string) error {
	sourceRoot := "mods/" + itemID
	err := fs.WalkDir(i.Payload, sourceRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(sourceRoot, path)
		if err != nil {
			return err
		}
		if filepath.ToSlash(relative) == engineTypes {
			return fs.SkipDir
		}
		if relative == engineTSConfig {
			return nil
		}
		if entry.Name() == ".DS_Store" {
			return nil
		}
		target := filepath.Join(staging, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fs.ReadFile(i.Payload, path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		return fmt.Errorf("stage payload: %w", err)
	}
	markerData, err := json.MarshalIndent(marker{ManagedBy: "vybava", ItemID: itemID}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(staging, ".vybava-package.json"), append(markerData, '\n'), 0o644); err != nil {
		return fmt.Errorf("write package marker: %w", err)
	}
	return nil
}

// carryEngineTypes moves the declarations the engine laid beside the prior
// copy into the new one. When the engine already laid fresh ones there (a
// hot reload won the race), those stand.
func carryEngineTypes(aside, destination string) error {
	from := filepath.Join(aside, filepath.FromSlash(engineTypes))
	if _, err := os.Stat(from); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	to := filepath.Join(destination, filepath.FromSlash(engineTypes))
	if _, err := os.Stat(to); err == nil {
		return nil
	}
	if err := os.Rename(from, to); err != nil {
		if _, statErr := os.Stat(to); statErr == nil {
			// A hot reload laid fresh types between the Stat and the Rename.
			return nil
		}
		return fmt.Errorf("carry engine types: %w", err)
	}
	return nil
}

func moveAside(root, itemID, destination string) (string, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("create stage directory: %w", err)
	}
	holder, err := os.MkdirTemp(root, itemID+"-removed-*")
	if err != nil {
		return "", fmt.Errorf("create staging directory: %w", err)
	}
	aside := filepath.Join(holder, itemID)
	if err := os.Rename(destination, aside); err != nil {
		_ = os.Remove(holder)
		return "", fmt.Errorf("move mod aside: %w", err)
	}
	return holder, nil
}

func (i Installer) stageRoot() (string, error) {
	if i.StageDir != "" {
		return i.StageDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home: %w", err)
	}
	return filepath.Join(home, ".cache", "vybava", "stage"), nil
}

// fallbackStageRoot sits beside the skills folder (~/.claude/.vybava-stage),
// on the destination's own volume and outside the folder the engine watches.
func fallbackStageRoot(destination string) string {
	return filepath.Join(filepath.Dir(filepath.Dir(destination)), ".vybava-stage")
}

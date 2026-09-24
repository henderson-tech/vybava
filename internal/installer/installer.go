package installer

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/catalog"
	statepkg "github.com/henderson-tech/vybava/internal/state"
)

type Agent string

const (
	AgentAll    Agent = "all"
	AgentClaude Agent = "claude"
	AgentCodex  Agent = "codex"
)

type Scope string

const (
	ScopeUser    Scope = "user"
	ScopeProject Scope = "project"
)

type Options struct {
	Agent   Agent
	Scope   Scope
	BinDir  string
	RootDir string
	DryRun  bool
}

type Operation struct {
	ItemID      string `json:"item_id"`
	Kind        string `json:"kind"`
	Agent       string `json:"agent,omitempty"`
	Scope       string `json:"scope"`
	Destination string `json:"destination"`
	Action      string `json:"action"`
}

type Installer struct {
	Payload fs.FS
	Store   statepkg.Store
	Now     func() time.Time
}

type marker struct {
	ManagedBy string `json:"managed_by"`
	ItemID    string `json:"item_id"`
}

func (i Installer) Plan(items []catalog.Item, options Options) ([]Operation, error) {
	if options.Agent == "" {
		options.Agent = AgentAll
	}
	if options.Scope == "" {
		options.Scope = ScopeUser
	}
	if options.Agent != AgentAll && options.Agent != AgentClaude && options.Agent != AgentCodex {
		return nil, fmt.Errorf("invalid agent %q", options.Agent)
	}
	if options.Scope != ScopeUser && options.Scope != ScopeProject {
		return nil, fmt.Errorf("invalid scope %q", options.Scope)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home: %w", err)
	}
	root := options.RootDir
	if root == "" {
		root, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("resolve current directory: %w", err)
		}
	}
	binDir := options.BinDir
	if binDir == "" {
		binDir = filepath.Join(home, ".local", "bin")
	}

	var operations []Operation
	for _, item := range items {
		switch item.Kind {
		case catalog.KindTool:
			// Tools install through their own channels (internal/toolsetup)
			// and are probed live, never recorded here.
			continue
		case catalog.KindApplet:
			operations = append(operations, Operation{
				ItemID: item.ID, Kind: string(item.Kind), Scope: string(options.Scope),
				Destination: filepath.Join(binDir, item.Applet), Action: "link applet",
			})
		case catalog.KindSkill:
			for _, agent := range selectedAgents(options.Agent) {
				base := skillBase(agent, options.Scope, home, root)
				operations = append(operations, Operation{
					ItemID: item.ID, Kind: string(item.Kind), Agent: string(agent), Scope: string(options.Scope),
					Destination: filepath.Join(base, item.ID), Action: "install skill",
				})
			}
		default:
			return nil, fmt.Errorf("item %q has unsupported kind %q", item.ID, item.Kind)
		}
	}
	return operations, nil
}

func (i Installer) Apply(operations []Operation, dryRun bool) error {
	if dryRun {
		return nil
	}
	currentState, err := i.Store.Load()
	if err != nil {
		return err
	}
	now := time.Now
	if i.Now != nil {
		now = i.Now
	}

	for _, operation := range operations {
		switch operation.Kind {
		case string(catalog.KindApplet):
			if err := installApplet(operation.Destination); err != nil {
				return fmt.Errorf("install %s: %w", operation.ItemID, err)
			}
		case string(catalog.KindSkill):
			if err := i.installSkill(operation.ItemID, operation.Destination); err != nil {
				return fmt.Errorf("install %s for %s: %w", operation.ItemID, operation.Agent, err)
			}
		default:
			return fmt.Errorf("unsupported operation kind %q", operation.Kind)
		}
		statepkg.Upsert(&currentState, statepkg.Installed{
			ItemID: operation.ItemID, Kind: operation.Kind, Agent: operation.Agent,
			Scope: operation.Scope, Destination: operation.Destination, InstalledAt: now().UTC(),
		})
	}
	return i.Store.Save(currentState)
}

func (i Installer) Remove(operations []Operation, dryRun bool) error {
	currentState, err := i.Store.Load()
	if err != nil {
		return err
	}
	for _, operation := range operations {
		if !dryRun {
			switch operation.Kind {
			case string(catalog.KindApplet):
				info, err := os.Lstat(operation.Destination)
				if err == nil {
					if runtime.GOOS != "windows" && info.Mode()&os.ModeSymlink == 0 {
						return fmt.Errorf("refusing to remove non-link applet at %s", operation.Destination)
					}
					if err := os.Remove(operation.Destination); err != nil {
						return fmt.Errorf("remove applet %s: %w", operation.ItemID, err)
					}
				} else if !errors.Is(err, os.ErrNotExist) {
					return err
				}
			case string(catalog.KindSkill):
				if err := ensureReplaceable(operation.Destination, operation.ItemID); err != nil {
					if errors.Is(err, os.ErrNotExist) {
						break
					}
					return err
				}
				if err := os.RemoveAll(operation.Destination); err != nil {
					return fmt.Errorf("remove skill %s: %w", operation.ItemID, err)
				}
			}
		}
		statepkg.Remove(&currentState, operation.ItemID, operation.Destination)
	}
	if dryRun {
		return nil
	}
	return i.Store.Save(currentState)
}

func (i Installer) installSkill(itemID, destination string) error {
	if err := ensureReplaceable(destination, itemID); err != nil {
		return err
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create skill home: %w", err)
	}
	staging, err := os.MkdirTemp(parent, ".vybava-"+itemID+"-*")
	if err != nil {
		return fmt.Errorf("create staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	sourceRoot := "skills/" + itemID
	err = fs.WalkDir(i.Payload, sourceRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(sourceRoot, path)
		if err != nil {
			return err
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
	if _, err := os.Stat(destination); err == nil {
		if err := os.RemoveAll(destination); err != nil {
			return fmt.Errorf("remove prior managed skill: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(staging, destination); err != nil {
		return fmt.Errorf("activate skill: %w", err)
	}
	return nil
}

func ensureReplaceable(destination, itemID string) error {
	info, err := os.Stat(destination)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("destination exists and is not a managed directory: %s", destination)
	}
	data, err := os.ReadFile(filepath.Join(destination, ".vybava-package.json"))
	if err != nil {
		return fmt.Errorf("refusing to replace unmanaged skill at %s", destination)
	}
	var existing marker
	if json.Unmarshal(data, &existing) != nil || existing.ManagedBy != "vybava" || existing.ItemID != itemID {
		return fmt.Errorf("refusing to replace unmanaged skill at %s", destination)
	}
	return nil
}

func installApplet(destination string) error {
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve current executable: %w", err)
	}
	return installAppletFrom(executable, destination)
}

func installAppletFrom(executable, destination string) error {
	resolvedExecutable, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return fmt.Errorf("resolve executable links: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return fmt.Errorf("create bin directory: %w", err)
	}
	if info, err := os.Lstat(destination); err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			return fmt.Errorf("refusing to replace non-link executable at %s", destination)
		}
		target, readErr := filepath.EvalSymlinks(destination)
		if readErr == nil && target != resolvedExecutable {
			return fmt.Errorf("refusing to replace link owned by another executable at %s", destination)
		}
		if err := os.Remove(destination); err != nil {
			return fmt.Errorf("replace existing link: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if runtime.GOOS == "windows" {
		return copyExecutable(resolvedExecutable, destination)
	}
	if err := os.Symlink(executable, destination); err != nil {
		return fmt.Errorf("create applet link: %w", err)
	}
	return nil
}

func copyExecutable(source, destination string) error {
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return os.WriteFile(destination, data, 0o755)
}

func selectedAgents(agent Agent) []Agent {
	if agent == AgentAll {
		return []Agent{AgentClaude, AgentCodex}
	}
	return []Agent{agent}
}

func skillBase(agent Agent, scope Scope, home, root string) string {
	prefix := root
	if scope == ScopeUser {
		prefix = home
	}
	return filepath.Join(prefix, "."+strings.ToLower(string(agent)), "skills")
}

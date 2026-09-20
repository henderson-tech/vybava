package memo

import (
	"os"
	"path/filepath"
)

// VaultEntry is one symlink in the Obsidian vault and what happened to it.
type VaultEntry struct {
	Alias  string `json:"alias"`
	Target string `json:"target"`
	Action string `json:"action"` // created | updated | kept
}

// VaultReport is the outcome of one `memo vault` run.
type VaultReport struct {
	Path    string       `json:"path"`
	Entries []VaultEntry `json:"entries"`
	Config  string       `json:"config"` // created | updated | kept
}

const obsidianApp = "{\n  \"useMarkdownLinks\": false,\n  \"newLinkFormat\": \"shortest\",\n  \"showUnsupportedFiles\": true\n}\n"

// Vault maintains `<path>/<alias> -> <home>` symlinks plus a minimal
// .obsidian/app.json. Idempotent: a correct link is kept, a wrong one is
// replaced, nothing else in the directory is touched.
func Vault(path string, homes []Home) (VaultReport, error) {
	report := VaultReport{Path: path, Entries: []VaultEntry{}}
	if err := os.MkdirAll(filepath.Join(path, ".obsidian"), 0o755); err != nil {
		return report, err
	}
	appJSON := filepath.Join(path, ".obsidian", "app.json")
	have, err := os.ReadFile(appJSON)
	switch {
	case os.IsNotExist(err):
		report.Config = "created"
	case err != nil:
		return report, err
	case string(have) == obsidianApp:
		report.Config = "kept"
	default:
		report.Config = "updated"
	}
	if report.Config != "kept" {
		if err := os.WriteFile(appJSON, []byte(obsidianApp), 0o644); err != nil {
			return report, err
		}
	}
	for _, h := range homes {
		link := filepath.Join(path, h.Alias)
		entry := VaultEntry{Alias: h.Alias, Target: h.Path}
		current, err := os.Readlink(link)
		switch {
		case err == nil && current == h.Path:
			entry.Action = "kept"
		case err == nil:
			if err := os.Remove(link); err != nil {
				return report, err
			}
			entry.Action = "updated"
		case os.IsNotExist(err):
			entry.Action = "created"
		default:
			return report, err
		}
		if entry.Action != "kept" {
			if err := os.Symlink(h.Path, link); err != nil {
				return report, err
			}
		}
		report.Entries = append(report.Entries, entry)
	}
	return report, nil
}

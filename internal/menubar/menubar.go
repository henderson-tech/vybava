// Package menubar diagnoses and repairs macOS menu-bar items that run but
// never appear.
//
// Since macOS 26, Control Center hosts every third-party NSStatusItem and
// files each one under the *responsible process* of whatever launched the
// app — not under the app itself. An app started from a terminal (`open -a`,
// a build script, `xcodebuild`, an agent shell) is therefore filed under the
// terminal's bundle id, and if that terminal's "Allow in the Menu Bar" switch
// in System Settings → Menu Bar is off, every item filed under it is silently
// never hosted: the process runs, its status-item window is parked at a screen
// edge or under the clock, and nothing is drawn.
//
// The attribution is persisted, which is what makes the symptom so
// disorienting: relaunching the app, reinstalling it, restarting Control
// Center, changing displays and even logging out all leave it invisible,
// because the mapping is on disk in the Control Center group container:
//
//	~/Library/Group Containers/group.com.apple.controlcenter/Library/
//	    Preferences/group.com.apple.controlcenter.plist
//
// Its `trackedApplications` key holds a *nested* binary plist — a Swift
// dictionary keyed by a non-string type, so it encodes as a flat array of
// alternating key/value entries. Each value carries `isAllowed` (the switch in
// the settings pane), `location` (the owning process) and `menuItemLocations`
// (every status item filed under that owner). An item whose location differs
// from its owner's is a *foreign* item; a foreign item under an owner that is
// not allowed never renders.
//
// Repair is to strip the foreign mappings so each app is attributed to itself
// again, flush the preferences daemon, restart Control Center, and relaunch
// the app outside the terminal's process tree (Launch).
package menubar

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"howett.net/plist"
)

// RegistryKey is the preferences key holding the nested menu-bar registry.
const RegistryKey = "trackedApplications"

// Env is the machine the doctor runs against; tests substitute it.
type Env struct {
	Home      string
	GOOS      string
	Now       time.Time
	ReadFile  func(name string) ([]byte, error)
	WriteFile func(name string, data []byte, perm fs.FileMode) error
	MkdirAll  func(path string, perm fs.FileMode) error
	Exec      func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// RegistryPath is the plist Control Center persists menu-bar attribution in.
func RegistryPath(home string) string {
	return filepath.Join(home, "Library", "Group Containers", "group.com.apple.controlcenter",
		"Library", "Preferences", "group.com.apple.controlcenter.plist")
}

// domainPath is what `defaults import` takes: the plist path without its
// extension. Writing the file directly would be overwritten by the running
// preferences daemon's cached copy.
func domainPath(home string) string {
	return strings.TrimSuffix(RegistryPath(home), ".plist")
}

// Owner is one row of the registry: a process that owns menu-bar items, and
// whether the user allows it in the menu bar at all.
type Owner struct {
	ID      string   `json:"id"`
	Allowed bool     `json:"allowed"`
	Items   []string `json:"items"`
}

// Finding is a status item filed under a different app than the one that owns
// it. Blocked means that owner is switched off, so the item never appears.
type Finding struct {
	Item    string `json:"item"`
	Owner   string `json:"owner"`
	Blocked bool   `json:"blocked"`
}

// Registry is the decoded menu-bar attribution table. The raw entries are kept
// so a repair rewrites only the mappings it strips and preserves every key
// macOS put there, including shapes this package does not model.
type Registry struct {
	entries []any
}

// Parse decodes the nested registry plist.
func Parse(blob []byte) (*Registry, error) {
	var entries []any
	if _, err := plist.Unmarshal(blob, &entries); err != nil {
		return nil, fmt.Errorf("decode %s: %w", RegistryKey, err)
	}
	return &Registry{entries: entries}, nil
}

// Encode re-marshals the registry as the binary plist macOS expects.
func (r *Registry) Encode() ([]byte, error) {
	return plist.Marshal(r.entries, plist.BinaryFormat)
}

// Owners lists every tracked process and the items filed under it.
func (r *Registry) Owners() []Owner {
	var owners []Owner
	r.each(func(id string, allowed bool, items []any) bool {
		owner := Owner{ID: id, Allowed: allowed}
		for _, item := range items {
			if got, ok := locationID(item); ok {
				owner.Items = append(owner.Items, got)
			}
		}
		owners = append(owners, owner)
		return false
	})
	sort.Slice(owners, func(i, j int) bool { return owners[i].ID < owners[j].ID })
	return owners
}

// Findings reports every item attributed to a foreign owner, blocked ones
// first — those are the invisible menu-bar items.
func (r *Registry) Findings() []Finding {
	var findings []Finding
	for _, owner := range r.Owners() {
		for _, item := range owner.Items {
			if item == owner.ID {
				continue
			}
			findings = append(findings, Finding{Item: item, Owner: owner.ID, Blocked: !owner.Allowed})
		}
	}
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Blocked != findings[j].Blocked {
			return findings[i].Blocked
		}
		return findings[i].Item < findings[j].Item
	})
	return findings
}

// Strip removes the given items from every owner they do not belong to and
// reports how many mappings went. An item's own row is never touched.
func (r *Registry) Strip(items []string) int {
	wanted := make(map[string]bool, len(items))
	for _, item := range items {
		wanted[item] = true
	}
	stripped := 0
	r.each(func(id string, _ bool, locations []any) bool {
		kept := make([]any, 0, len(locations))
		for _, location := range locations {
			got, ok := locationID(location)
			if ok && got != id && wanted[got] {
				stripped++
				continue
			}
			kept = append(kept, location)
		}
		if len(kept) == len(locations) {
			return false
		}
		r.setItems(id, kept)
		return false
	})
	return stripped
}

// each walks the alternating key/value array, calling fn with each owner's id,
// switch state and raw item locations.
func (r *Registry) each(fn func(id string, allowed bool, items []any) bool) {
	for index := 1; index < len(r.entries); index += 2 {
		record, ok := r.entries[index].(map[string]any)
		if !ok {
			continue
		}
		id, ok := locationID(record["location"])
		if !ok {
			continue
		}
		allowed, _ := record["isAllowed"].(bool)
		items, _ := record["menuItemLocations"].([]any)
		if fn(id, allowed, items) {
			return
		}
	}
}

func (r *Registry) setItems(id string, items []any) {
	for index := 1; index < len(r.entries); index += 2 {
		record, ok := r.entries[index].(map[string]any)
		if !ok {
			continue
		}
		if got, ok := locationID(record["location"]); ok && got == id {
			record["menuItemLocations"] = items
			return
		}
	}
}

// locationID reads Control Center's Location enum: a single-key map whose
// payload is the bundle id (`bundle`) or a file URL (`adhocBinary`, used for
// unbundled binaries such as a test build or a toolchain helper).
func locationID(value any) (string, bool) {
	wrapper, ok := value.(map[string]any)
	if !ok || len(wrapper) != 1 {
		return "", false
	}
	for _, payload := range wrapper {
		fields, ok := payload.(map[string]any)
		if !ok {
			return "", false
		}
		switch inner := fields["_0"].(type) {
		case string:
			return inner, inner != ""
		case map[string]any:
			url, ok := inner["relative"].(string)
			return url, ok && url != ""
		}
	}
	return "", false
}

// Report is what a scan found.
type Report struct {
	Path     string    `json:"path"`
	Owners   []Owner   `json:"owners"`
	Findings []Finding `json:"findings"`
}

// Blocked is the subset of findings that are actually invisible right now.
func (r Report) Blocked() []Finding {
	var blocked []Finding
	for _, finding := range r.Findings {
		if finding.Blocked {
			blocked = append(blocked, finding)
		}
	}
	return blocked
}

// ErrUnsupported is returned off macOS, where the registry does not exist.
var ErrUnsupported = errors.New("menu-bar attribution is a macOS concept")

// Scan reads the registry and reports foreign attributions. It changes nothing.
func Scan(env Env) (Report, error) {
	if env.GOOS != "darwin" {
		return Report{}, fmt.Errorf("%w; this %s host has no Control Center registry", ErrUnsupported, env.GOOS)
	}
	path := RegistryPath(env.Home)
	raw, err := env.ReadFile(path)
	if err != nil {
		return Report{}, fmt.Errorf("read %s: %w", path, err)
	}
	registry, _, err := decode(raw)
	if err != nil {
		return Report{}, err
	}
	return Report{Path: path, Owners: registry.Owners(), Findings: registry.Findings()}, nil
}

func decode(raw []byte) (*Registry, map[string]any, error) {
	var outer map[string]any
	if _, err := plist.Unmarshal(raw, &outer); err != nil {
		return nil, nil, fmt.Errorf("decode registry: %w", err)
	}
	blob, ok := outer[RegistryKey].([]byte)
	if !ok {
		return nil, nil, fmt.Errorf("registry has no %s data — nothing has been tracked on this machine yet", RegistryKey)
	}
	registry, err := Parse(blob)
	if err != nil {
		return nil, nil, err
	}
	return registry, outer, nil
}

// FixResult is what a repair did.
type FixResult struct {
	Path     string    `json:"path"`
	Backup   string    `json:"backup"`
	Stripped []Finding `json:"stripped"`
	Restored []string  `json:"restored"`
}

// Fix strips foreign attributions so each app is filed under itself again,
// then flushes the preferences daemon and restarts Control Center. Only the
// blocked findings are repaired unless all is set, because a foreign item
// under an *allowed* owner is currently visible and stripping it would make it
// disappear until its app is relaunched.
//
// The apps themselves still have to be relaunched outside the terminal's
// process tree — Restored names them, and Launch does it.
func Fix(ctx context.Context, env Env, all bool) (FixResult, error) {
	report, err := Scan(env)
	if err != nil {
		return FixResult{}, err
	}
	targets := report.Blocked()
	if all {
		targets = report.Findings
	}
	if len(targets) == 0 {
		return FixResult{Path: report.Path}, nil
	}

	raw, err := env.ReadFile(report.Path)
	if err != nil {
		return FixResult{}, fmt.Errorf("read %s: %w", report.Path, err)
	}
	registry, outer, err := decode(raw)
	if err != nil {
		return FixResult{}, err
	}

	backup, err := writeBackup(env, report.Path, raw)
	if err != nil {
		return FixResult{}, err
	}

	items := make([]string, 0, len(targets))
	for _, finding := range targets {
		items = append(items, finding.Item)
	}
	if registry.Strip(items) == 0 {
		return FixResult{Path: report.Path, Backup: backup}, nil
	}

	blob, err := registry.Encode()
	if err != nil {
		return FixResult{}, err
	}
	outer[RegistryKey] = blob
	updated, err := plist.Marshal(outer, plist.BinaryFormat)
	if err != nil {
		return FixResult{}, err
	}

	staged := filepath.Join(filepath.Dir(backup), "staged.plist")
	if err := env.WriteFile(staged, updated, 0o600); err != nil {
		return FixResult{}, fmt.Errorf("stage the rewritten registry: %w", err)
	}
	// `defaults import` writes through the running preferences daemon; writing
	// the file underneath it would be discarded by its cached copy.
	if out, err := env.Exec(ctx, "defaults", "import", domainPath(env.Home), staged); err != nil {
		return FixResult{}, fmt.Errorf("defaults import: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// SIGTERM so the daemon flushes; Control Center is killed hard and is
	// brought straight back by launchd, reading the registry from disk.
	_, _ = env.Exec(ctx, "killall", "cfprefsd")
	_, _ = env.Exec(ctx, "killall", "-9", "ControlCenter")

	return FixResult{Path: report.Path, Backup: backup, Stripped: targets, Restored: dedupe(items)}, nil
}

func writeBackup(env Env, path string, raw []byte) (string, error) {
	dir := filepath.Join(env.Home, "Backups", "menubar-doctor")
	if err := env.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create the backup directory: %w", err)
	}
	backup := filepath.Join(dir, fmt.Sprintf("%s.%s.plist",
		strings.TrimSuffix(filepath.Base(path), ".plist"), env.Now.UTC().Format("20060102-150405")))
	if err := env.WriteFile(backup, raw, 0o600); err != nil {
		return "", fmt.Errorf("back up the registry: %w", err)
	}
	return backup, nil
}

func dedupe(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

// Launch starts a menu-bar app detached from this process, so Control Center
// attributes its status item to the app itself rather than to the terminal.
// It takes a path to an .app bundle or to an executable.
func Launch(ctx context.Context, env Env, target string) (string, error) {
	if env.GOOS != "darwin" {
		return "", fmt.Errorf("%w; this %s host has no menu bar", ErrUnsupported, env.GOOS)
	}
	binary, err := executable(env, target)
	if err != nil {
		return "", err
	}
	label := "vybava.menubar." + strings.NewReplacer(" ", "-", "/", "-").Replace(filepath.Base(binary))
	// A label left over from an earlier launch would make submit fail.
	_, _ = env.Exec(ctx, "launchctl", "remove", label)
	if out, err := env.Exec(ctx, "launchctl", "submit", "-l", label, "--", binary); err != nil {
		return "", fmt.Errorf("launchctl submit: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return label, nil
}

// executable resolves an .app bundle to the binary inside it.
func executable(env Env, target string) (string, error) {
	target = strings.TrimSuffix(target, "/")
	if filepath.Ext(target) != ".app" {
		return target, nil
	}
	raw, err := env.ReadFile(filepath.Join(target, "Contents", "Info.plist"))
	if err != nil {
		return "", fmt.Errorf("read the bundle's Info.plist: %w", err)
	}
	var info map[string]any
	if _, err := plist.Unmarshal(raw, &info); err != nil {
		return "", fmt.Errorf("decode the bundle's Info.plist: %w", err)
	}
	name, ok := info["CFBundleExecutable"].(string)
	if !ok || name == "" {
		return "", fmt.Errorf("%s declares no CFBundleExecutable", target)
	}
	return filepath.Join(target, "Contents", "MacOS", name), nil
}

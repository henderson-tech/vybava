package menubar_test

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/henderson-tech/vybava/internal/menubar"
	"howett.net/plist"
)

func bundle(id string) map[string]any {
	return map[string]any{"bundle": map[string]any{"_0": id}}
}

func adhoc(url string) map[string]any {
	return map[string]any{"adhocBinary": map[string]any{"_0": map[string]any{"relative": url}}}
}

// registry builds the alternating key/value array macOS stores: a Swift
// dictionary keyed by a non-string type encodes as [key, value, key, value…].
func registry(rows ...[]any) []byte {
	var entries []any
	for _, row := range rows {
		owner, allowed, items := row[0], row[1], row[2].([]any)
		entries = append(entries, owner, map[string]any{
			"isAllowed": allowed, "location": owner, "menuItemLocations": items,
		})
	}
	blob, err := plist.Marshal(entries, plist.BinaryFormat)
	if err != nil {
		panic(err)
	}
	return blob
}

// theTerminal owns three items although only one is its own, and its menu-bar
// switch is off — the shape that makes shell-launched apps invisible.
func fixture() []byte {
	return registry(
		[]any{bundle("dev.terminal.app"), false, []any{bundle("dev.terminal.app"), bundle("com.example.bar"), bundle("com.example.snap")}},
		[]any{bundle("com.example.host"), true, []any{bundle("com.example.host"), bundle("com.example.adopted")}},
		[]any{bundle("com.example.fine"), true, []any{bundle("com.example.fine")}},
		[]any{adhoc("file:///usr/local/bin/probe"), true, []any{adhoc("file:///usr/local/bin/probe")}},
	)
}

func outer(blob []byte) []byte {
	raw, err := plist.Marshal(map[string]any{menubar.RegistryKey: blob, "unrelated": "kept"}, plist.BinaryFormat)
	if err != nil {
		panic(err)
	}
	return raw
}

func TestFindingsNameForeignItemsAndFlagTheInvisibleOnes(t *testing.T) {
	reg, err := menubar.Parse(fixture())
	if err != nil {
		t.Fatal(err)
	}

	findings := reg.Findings()
	if len(findings) != 3 {
		t.Fatalf("want 3 foreign attributions, got %d: %+v", len(findings), findings)
	}
	if !findings[0].Blocked || !findings[1].Blocked {
		t.Errorf("blocked findings must sort first, got %+v", findings)
	}
	last := findings[2]
	if last.Item != "com.example.adopted" || last.Owner != "com.example.host" || last.Blocked {
		t.Errorf("an item under an allowed owner is visible, not blocked: %+v", last)
	}
	for _, owner := range reg.Owners() {
		if owner.ID == "file:///usr/local/bin/probe" && len(owner.Items) != 1 {
			t.Errorf("unbundled binaries are tracked by URL: %+v", owner)
		}
	}
}

func TestStripRemovesOnlyTheForeignMappingsAsked(t *testing.T) {
	reg, err := menubar.Parse(fixture())
	if err != nil {
		t.Fatal(err)
	}

	// "com.example.fine" is named but owns its own row: it must survive.
	if stripped := reg.Strip([]string{"com.example.bar", "com.example.fine"}); stripped != 1 {
		t.Fatalf("want 1 mapping stripped, got %d", stripped)
	}

	encoded, err := reg.Encode()
	if err != nil {
		t.Fatal(err)
	}
	after, err := menubar.Parse(encoded)
	if err != nil {
		t.Fatal(err)
	}
	for _, owner := range after.Owners() {
		switch owner.ID {
		case "dev.terminal.app":
			if strings.Join(owner.Items, ",") != "dev.terminal.app,com.example.snap" {
				t.Errorf("stripped the wrong mappings: %+v", owner.Items)
			}
		case "com.example.fine":
			if len(owner.Items) != 1 {
				t.Errorf("an owner's own item is never stripped: %+v", owner.Items)
			}
		}
	}
}

func TestScanRefusesOffMacOS(t *testing.T) {
	_, err := menubar.Scan(menubar.Env{GOOS: "linux"})
	if !errors.Is(err, menubar.ErrUnsupported) {
		t.Fatalf("want ErrUnsupported, got %v", err)
	}
}

type recorder struct {
	files map[string][]byte
	runs  [][]string
}

func (r *recorder) env() menubar.Env {
	return menubar.Env{
		Home: "/home", GOOS: "darwin", Now: time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC),
		ReadFile: func(name string) ([]byte, error) {
			data, ok := r.files[name]
			if !ok {
				return nil, fs.ErrNotExist
			}
			return data, nil
		},
		WriteFile: func(name string, data []byte, _ fs.FileMode) error {
			r.files[name] = data
			return nil
		},
		MkdirAll: func(string, fs.FileMode) error { return nil },
		Exec: func(_ context.Context, name string, args ...string) ([]byte, error) {
			r.runs = append(r.runs, append([]string{name}, args...))
			return nil, nil
		},
	}
}

func TestFixBacksUpStripsTheBlockedMappingsAndRestartsControlCenter(t *testing.T) {
	rec := &recorder{files: map[string][]byte{menubar.RegistryPath("/home"): outer(fixture())}}

	result, err := menubar.Fix(context.Background(), rec.env(), false)
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Stripped) != 2 || strings.Join(result.Restored, ",") != "com.example.bar,com.example.snap" {
		t.Fatalf("only the invisible items are repaired: %+v", result)
	}
	if want := filepath.Join("/home", "Backups", "menubar-doctor", "group.com.apple.controlcenter.20260914-120000.plist"); result.Backup != want {
		t.Errorf("backup path %q, want %q", result.Backup, want)
	}
	if _, ok := rec.files[result.Backup]; !ok {
		t.Error("the original registry is written to the backup before any rewrite")
	}

	staged, ok := rec.files[filepath.Join(filepath.Dir(result.Backup), "staged.plist")]
	if !ok {
		t.Fatal("the rewritten registry is staged for defaults import")
	}
	var written map[string]any
	if _, err := plist.Unmarshal(staged, &written); err != nil {
		t.Fatal(err)
	}
	if written["unrelated"] != "kept" {
		t.Error("keys the doctor does not model must survive the rewrite")
	}
	after, err := menubar.Parse(written[menubar.RegistryKey].([]byte))
	if err != nil {
		t.Fatal(err)
	}
	for _, finding := range after.Findings() {
		if finding.Blocked {
			t.Errorf("blocked attribution survived the fix: %+v", finding)
		}
	}
	if len(after.Findings()) != 1 {
		t.Errorf("a visible foreign item is left alone without --all: %+v", after.Findings())
	}

	var commands []string
	for _, run := range rec.runs {
		commands = append(commands, run[0]+" "+strings.Join(run[1:], " "))
	}
	joined := strings.Join(commands, "\n")
	if !strings.Contains(joined, "defaults import /home/Library/Group Containers/group.com.apple.controlcenter/Library/Preferences/group.com.apple.controlcenter") {
		t.Errorf("the rewrite must go through defaults import, not a raw file write:\n%s", joined)
	}
	if !strings.Contains(joined, "killall cfprefsd") || !strings.Contains(joined, "killall -9 ControlCenter") {
		t.Errorf("the daemons must be restarted so the change takes effect:\n%s", joined)
	}
}

func TestFixIsANoOpWhenNothingIsBlocked(t *testing.T) {
	clean := registry([]any{bundle("com.example.fine"), true, []any{bundle("com.example.fine")}})
	rec := &recorder{files: map[string][]byte{menubar.RegistryPath("/home"): outer(clean)}}

	result, err := menubar.Fix(context.Background(), rec.env(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Stripped) != 0 || result.Backup != "" || len(rec.runs) != 0 {
		t.Fatalf("a healthy machine is left untouched: %+v, ran %v", result, rec.runs)
	}
}

func TestFixAllAlsoUnfilesItemsThatAreVisibleUnderAForeignOwner(t *testing.T) {
	rec := &recorder{files: map[string][]byte{menubar.RegistryPath("/home"): outer(fixture())}}

	result, err := menubar.Fix(context.Background(), rec.env(), true)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Join(result.Restored, ",") != "com.example.adopted,com.example.bar,com.example.snap" {
		t.Fatalf("--all repairs every foreign attribution: %+v", result.Restored)
	}
	staged := rec.files[filepath.Join(filepath.Dir(result.Backup), "staged.plist")]
	var written map[string]any
	if _, err := plist.Unmarshal(staged, &written); err != nil {
		t.Fatal(err)
	}
	after, err := menubar.Parse(written[menubar.RegistryKey].([]byte))
	if err != nil {
		t.Fatal(err)
	}
	if len(after.Findings()) != 0 {
		t.Errorf("no foreign attribution should survive --all: %+v", after.Findings())
	}
}

func TestFixSurfacesADefaultsImportFailureAndLeavesControlCenterAlone(t *testing.T) {
	rec := &recorder{files: map[string][]byte{menubar.RegistryPath("/home"): outer(fixture())}}
	env := rec.env()
	exec := env.Exec
	env.Exec = func(ctx context.Context, name string, args ...string) ([]byte, error) {
		_, _ = exec(ctx, name, args...)
		if name == "defaults" {
			return []byte("Domain … does not exist"), errors.New("exit status 1")
		}
		return nil, nil
	}

	_, err := menubar.Fix(context.Background(), env, false)
	if err == nil || !strings.Contains(err.Error(), "defaults import") {
		t.Fatalf("a failed import must be surfaced, not swallowed: %v", err)
	}
	for _, run := range rec.runs {
		if run[0] == "killall" {
			t.Errorf("Control Center is only restarted after the registry was actually written: %v", rec.runs)
		}
	}
}

func TestFixNeverRewritesTheRegistryWhenTheBackupCannotBeWritten(t *testing.T) {
	original := outer(fixture())
	rec := &recorder{files: map[string][]byte{menubar.RegistryPath("/home"): original}}
	env := rec.env()
	write := env.WriteFile
	env.WriteFile = func(name string, data []byte, perm fs.FileMode) error {
		if strings.Contains(name, "/Backups/") {
			return errors.New("read-only file system")
		}
		return write(name, data, perm)
	}

	_, err := menubar.Fix(context.Background(), env, false)
	if err == nil || !strings.Contains(err.Error(), "back up the registry") {
		t.Fatalf("no repair without a backup: %v", err)
	}
	if len(rec.runs) != 0 {
		t.Errorf("nothing may run before the backup exists: %v", rec.runs)
	}
	if len(rec.files) != 1 {
		t.Errorf("no file may be staged before the backup exists: %v", rec.files)
	}
}

func TestLaunchResolvesABundleAndDetachesItFromThisProcess(t *testing.T) {
	info, err := plist.Marshal(map[string]any{"CFBundleExecutable": "Demo"}, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	rec := &recorder{files: map[string][]byte{"/Applications/Demo.app/Contents/Info.plist": info}}

	label, err := menubar.Launch(context.Background(), rec.env(), "/Applications/Demo.app")
	if err != nil {
		t.Fatal(err)
	}
	if label != "vybava.menubar.Demo" {
		t.Errorf("label %q", label)
	}
	last := rec.runs[len(rec.runs)-1]
	if strings.Join(last, " ") != "launchctl submit -l vybava.menubar.Demo -- /Applications/Demo.app/Contents/MacOS/Demo" {
		t.Errorf("launch must reparent the app to launchd: %v", last)
	}
}

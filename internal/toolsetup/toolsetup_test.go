package toolsetup

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/henderson-tech/vybava/internal/catalog"
)

type fakeShelf struct {
	releases map[string]Release
	payload  map[string][]byte
}

func (f fakeShelf) Latest(app string) (Release, error) {
	r, ok := f.releases[app]
	if !ok {
		return Release{}, Diag{Code: DiagShelfMissing, Detail: app}
	}
	return r, nil
}

func (f fakeShelf) Download(r Release, dst string) error {
	return os.WriteFile(dst, f.payload[r.App], 0o644)
}

func shelfWith(app, artifact string, data []byte) fakeShelf {
	sum := sha256.Sum256(data)
	return fakeShelf{
		releases: map[string]Release{app: {App: app, Version: "1.2.0", Artifact: artifact, SHA256: hex.EncodeToString(sum[:]), URL: "/x"}},
		payload:  map[string][]byte{app: data},
	}
}

func testEnv(t *testing.T, shelf Shelf) Env {
	t.Helper()
	root := t.TempDir()
	return Env{
		Home: root, Arch: "arm64",
		AppDirs:  []string{filepath.Join(root, "Applications")},
		LookPath: func(string) (string, error) { return "", errors.New("absent") },
		Exec:     func([]string, bool) error { return nil },
		Output:   func([]string) (string, error) { return "", errors.New("no plist") },
		Shelf:    shelf, BinDir: filepath.Join(root, "bin"), TempRoot: root,
	}
}

func toolItem(id string, tool catalog.Tool) catalog.Item {
	return catalog.Item{ID: id, Kind: catalog.KindTool, Tool: &tool}
}

func TestOrderPutsNeedsFirstAndRejectsCycles(t *testing.T) {
	a := toolItem("a", catalog.Tool{Needs: []string{"b"}})
	b := toolItem("b", catalog.Tool{})
	ordered, err := Order([]catalog.Item{a, b})
	if err != nil || ordered[0].ID != "b" || ordered[1].ID != "a" {
		t.Fatalf("order = %v, %v; want b before a", ordered, err)
	}
	cyclic := toolItem("b", catalog.Tool{Needs: []string{"a"}})
	if _, err := Order([]catalog.Item{a, cyclic}); err == nil {
		t.Fatal("a needs cycle was accepted")
	}
}

func TestSelectionLeavesOptionalOutUnlessAsked(t *testing.T) {
	items := []catalog.Item{
		toolItem("onyx", catalog.Tool{}),
		toolItem("devbox", catalog.Tool{Optional: true}),
		{ID: "prm", Kind: catalog.KindSkill},
	}
	ids := func(items []catalog.Item) (out []string) {
		for _, i := range items {
			out = append(out, i.ID)
		}
		return out
	}
	if got, _ := Selection(items, nil, nil); len(got) != 2 || got[0].ID != "onyx" || got[1].ID != "prm" {
		t.Fatalf("default = %v; want onyx, prm", ids(got))
	}
	if got, _ := Selection(items, nil, []string{"devbox"}); len(got) != 3 {
		t.Fatalf("--with devbox = %v; want all three", ids(got))
	}
	if got, unknown := Selection(items, []string{"devbox", "nope"}, nil); len(got) != 1 || len(unknown) != 1 {
		t.Fatalf("--only = %v unknown %v; want [devbox] and [nope]", ids(got), unknown)
	}
}

func TestPultikInstallsVerifiedBinary(t *testing.T) {
	env := testEnv(t, shelfWith("tool-darwin-arm64", "tool", []byte("#!/bin/sh\n")))
	item := toolItem("tool", catalog.Tool{Probe: catalog.Probe{Command: "tool"}, Install: catalog.Install{Pultik: "tool-darwin-{arch}"}, Setup: [][]string{{"tool", "shell", "install"}}})
	res, err := Apply(env, item, Options{})
	if err != nil || res.Action != "installed" {
		t.Fatalf("apply = %+v, %v", res, err)
	}
	// ~/.local/bin may not be on PATH yet: the setup command runs by its probed path.
	if want := filepath.Join(env.BinDir, "tool"); res.SetupArgv[0][0] != want {
		t.Fatalf("setup argv = %v; want it to start with %s", res.SetupArgv[0], want)
	}
	info, err := os.Stat(filepath.Join(env.BinDir, "tool"))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("binary not installed executable: %v", err)
	}
	if res, err := Apply(env, item, Options{Update: true}); err != nil || res.Action != "current" {
		t.Fatalf("update of a matching binary = %+v, %v; want current", res, err)
	}
}

func TestPultikRefusesChecksumMismatch(t *testing.T) {
	shelf := shelfWith("tool", "tool", []byte("real"))
	shelf.payload["tool"] = []byte("tampered")
	env := testEnv(t, shelf)
	item := toolItem("tool", catalog.Tool{Probe: catalog.Probe{Command: "tool"}, Install: catalog.Install{Pultik: "tool"}})
	_, err := Apply(env, item, Options{})
	var diag Diag
	if !errors.As(err, &diag) || diag.Code != DiagChecksum {
		t.Fatalf("err = %v; want %s", err, DiagChecksum)
	}
	if _, statErr := os.Stat(filepath.Join(env.BinDir, "tool")); statErr == nil {
		t.Fatal("a tampered artifact was installed")
	}
}

func TestPultikPlacesAppFromZip(t *testing.T) {
	env := testEnv(t, shelfWith("demo", "Demo.zip", []byte("zip")))
	env.Exec = func(argv []string, _ bool) error {
		switch argv[0] {
		case "ditto":
			if argv[1] == "-xk" { // unzip: the archive holds Demo.app
				return os.MkdirAll(filepath.Join(argv[3], "Demo.app", "Contents"), 0o755)
			}
			return os.Rename(argv[1], argv[2])
		}
		return nil
	}
	item := toolItem("demo", catalog.Tool{Probe: catalog.Probe{App: "Demo.app"}, Install: catalog.Install{Pultik: "demo"}})
	if res, err := Apply(env, item, Options{}); err != nil || res.Action != "installed" {
		t.Fatalf("apply = %+v, %v", res, err)
	}
	if !isDir(filepath.Join(env.AppDirs[0], "Demo.app")) {
		t.Fatal("Demo.app not placed in the app dir")
	}
}

func TestApplyAllStopsDependentsOfAMissingNeed(t *testing.T) {
	env := testEnv(t, fakeShelf{})
	guided := toolItem("mcp", catalog.Tool{Probe: catalog.Probe{Path: "~/mcp"}, Install: catalog.Install{Run: []string{"setup"}}, Interactive: true})
	dependent := toolItem("ext", catalog.Tool{Probe: catalog.Probe{Path: "~/ext"}, Install: catalog.Install{Run: []string{"x"}}, Needs: []string{"mcp"}})
	all := map[string]catalog.Item{"mcp": guided, "ext": dependent}
	outcomes, err := ApplyAll(env, []catalog.Item{dependent, guided}, all, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if outcomes[0].Result.ID != "mcp" || outcomes[0].Diag.Code != DiagNeedsHuman {
		t.Fatalf("guided tool without a terminal = %+v", outcomes[0])
	}
	if outcomes[1].Result.ID != "ext" || outcomes[1].Diag.Code != DiagNeedsMissing {
		t.Fatalf("dependent of an uninstalled need = %+v", outcomes[1])
	}
}

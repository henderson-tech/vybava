package toolsetup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/catalog"
)

// ShelfURL is the pultik shelf every henderson-tech artifact ships to.
const ShelfURL = "https://apps.fixit.app"

// Release is one shelf app's latest artifact.
type Release struct {
	App      string `json:"app"`
	Version  string `json:"version"`
	Artifact string `json:"artifact"`
	SHA256   string `json:"sha256"`
	URL      string `json:"url"`
}

// Shelf resolves and downloads shelf artifacts.
type Shelf interface {
	Latest(app string) (Release, error)
	Download(release Release, dst string) error
}

// HTTPShelf reads the public shelf API; no credentials are involved.
type HTTPShelf struct {
	Base   string
	Client *http.Client
}

// NewHTTPShelf returns the production shelf client.
func NewHTTPShelf() HTTPShelf {
	return HTTPShelf{Base: ShelfURL, Client: &http.Client{Timeout: 10 * time.Minute}}
}

func (s HTTPShelf) Latest(app string) (Release, error) {
	resp, err := s.Client.Get(s.Base + "/api/apps")
	if err != nil {
		return Release{}, fmt.Errorf("read shelf: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("read shelf: HTTP %d", resp.StatusCode)
	}
	var listing struct {
		Apps []struct {
			ID     string   `json:"id"`
			Latest *Release `json:"latest"`
		} `json:"apps"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&listing); err != nil {
		return Release{}, fmt.Errorf("decode shelf: %w", err)
	}
	for _, a := range listing.Apps {
		if a.ID == app && a.Latest != nil {
			return *a.Latest, nil
		}
	}
	return Release{}, Diag{Code: DiagShelfMissing, Detail: "shelf has no published release of " + app, Fix: "open " + s.Base}
}

func (s HTTPShelf) Download(release Release, dst string) error {
	resp, err := s.Client.Get(s.Base + release.URL)
	if err != nil {
		return fmt.Errorf("download %s: %w", release.App, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", release.App, resp.StatusCode)
	}
	file, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, resp.Body); err != nil {
		file.Close()
		return fmt.Errorf("download %s: %w", release.App, err)
	}
	return file.Close()
}

// errCurrent tells Apply the installed copy already matches the shelf.
var errCurrent = errors.New("already current")

// pultik installs a shelf artifact: an .app from a .zip or .dmg into the
// first app dir, or a raw binary into BinDir. An update only replaces what
// differs from the shelf — app version string, or binary checksum.
func pultik(env Env, app string, tool catalog.Tool, present bool) (string, error) {
	release, err := env.Shelf.Latest(app)
	if err != nil {
		return "", err
	}
	if present && current(env, tool, release) {
		return release.Version, errCurrent
	}
	work, err := os.MkdirTemp(env.TempRoot, "vybava-tool-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(work)
	download := filepath.Join(work, filepath.Base(release.Artifact))
	if err := env.Shelf.Download(release, download); err != nil {
		return "", err
	}
	if sum, err := fileSHA256(download); err != nil {
		return "", err
	} else if !strings.EqualFold(sum, release.SHA256) {
		return "", Diag{Code: DiagChecksum, Detail: fmt.Sprintf("%s %s: sha256 %s, shelf says %s", app, release.Version, sum, release.SHA256), Fix: "vybava setup team --only " + app}
	}

	switch {
	case strings.HasSuffix(release.Artifact, ".zip"):
		unpacked := filepath.Join(work, "unpacked")
		if err := env.Exec([]string{"ditto", "-xk", download, unpacked}, false); err != nil {
			return "", fmt.Errorf("unzip %s: %w", release.Artifact, err)
		}
		return release.Version, placeApp(env, unpacked, tool.Probe.App, work)
	case strings.HasSuffix(release.Artifact, ".dmg"):
		mount := filepath.Join(work, "mount")
		if err := env.Exec([]string{"hdiutil", "attach", "-nobrowse", "-readonly", "-mountpoint", mount, download}, false); err != nil {
			return "", fmt.Errorf("mount %s: %w", release.Artifact, err)
		}
		defer func() { _ = env.Exec([]string{"hdiutil", "detach", "-quiet", mount}, false) }()
		return release.Version, placeApp(env, mount, tool.Probe.App, work)
	default:
		name := tool.Probe.Command
		if name == "" {
			name = filepath.Base(release.Artifact)
		}
		return release.Version, placeBinary(download, filepath.Join(env.BinDir, name))
	}
}

// current reports whether the installed copy already matches the shelf.
func current(env Env, tool catalog.Tool, release Release) bool {
	if tool.Probe.App != "" {
		_, where := Probe(env, tool)
		version, err := env.Output([]string{"defaults", "read", filepath.Join(where, "Contents", "Info"), "CFBundleShortVersionString"})
		return err == nil && strings.TrimSpace(version) == release.Version
	}
	_, where := Probe(env, tool)
	sum, err := fileSHA256(where)
	return err == nil && strings.EqualFold(sum, release.SHA256)
}

// placeApp copies the named bundle out of dir into the install dir, moving
// any previous copy aside first so a failed copy never leaves no app at all.
func placeApp(env Env, dir, bundle, work string) error {
	source := filepath.Join(dir, bundle)
	if !isDir(source) {
		return fmt.Errorf("artifact carries no %s", bundle)
	}
	target := filepath.Join(env.AppDirs[0], bundle)
	if err := os.MkdirAll(env.AppDirs[0], 0o755); err != nil {
		return err
	}
	if isDir(target) {
		if err := os.Rename(target, filepath.Join(work, "previous-"+bundle)); err != nil {
			return fmt.Errorf("move old %s aside: %w", bundle, err)
		}
	}
	// ditto keeps the code signature, xattrs and symlinks intact.
	if err := env.Exec([]string{"ditto", source, target}, false); err != nil {
		return fmt.Errorf("copy %s: %w", bundle, err)
	}
	return nil
}

func placeBinary(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	staged := dst + ".vybava-new"
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(staged, data, 0o755); err != nil {
		return err
	}
	return os.Rename(staged, dst)
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// ExecRunner runs argv on the real machine.
func ExecRunner(argv []string, interactive bool) error {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if interactive {
		cmd.Stdin, cmd.Stdout = os.Stdin, os.Stdout
	}
	return cmd.Run()
}

// ExecOutput runs argv and returns its stdout.
func ExecOutput(argv []string) (string, error) {
	out, err := exec.Command(argv[0], argv[1:]...).Output()
	return string(out), err
}

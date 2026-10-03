package buildindex

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/perflab/hostexec"
)

// The build serialisation protocol, two kernel locks under <state>/locks:
//
//   - host-build.lock: exclusive, held for a whole native build (one build at
//     a time Mac-wide). A measuring verb refuses while it is held.
//   - host-measure.lock: shared, held by every measuring verb (run, probe)
//     for its window. A build refuses while any holder has it.
//
// Each side takes its own lock first and then checks the other, so two
// racing starts can both refuse but can never both proceed. Holder records
// (<lock>.json, measure/<pid>.json) only name the holder; the kernel lock is
// the truth, so a crashed holder's leftover record is never believed.
const (
	hostBuildLock   = "host-build.lock"
	hostMeasureLock = "host-measure.lock"
	measureHolders  = "measure"
)

// LockHolder is what a held lock records about its holder.
type LockHolder struct {
	PID      int       `json:"pid"`
	Verb     string    `json:"verb"`
	Key      string    `json:"key,omitempty"`
	Device   string    `json:"device,omitempty"`
	Worktree string    `json:"worktree,omitempty"`
	Since    time.Time `json:"since"`
}

func (h LockHolder) String() string {
	parts := []string{fmt.Sprintf("pid %d", h.PID)}
	if h.Verb != "" {
		parts = append(parts, "`perflab "+h.Verb+"`")
	}
	if h.Key != "" {
		parts = append(parts, "key "+h.Key)
	}
	if h.Device != "" {
		parts = append(parts, "device "+h.Device)
	}
	if !h.Since.IsZero() {
		parts = append(parts, "for "+time.Since(h.Since).Round(time.Second).String())
	}
	return strings.Join(parts, ", ")
}

// takeExclusive acquires <locks>/<name> within wait and records holder in
// <name>.json. On a timeout it returns the current holder (best effort) and
// errLockTimeout.
func (d Dirs) takeExclusive(name string, wait time.Duration, holder LockHolder) (func(), *LockHolder, error) {
	if err := os.MkdirAll(d.locksDir(), 0o755); err != nil {
		return nil, nil, err
	}
	path := filepath.Join(d.locksDir(), name)
	_, unlock, err := lockFile(path, wait, false)
	if errors.Is(err, errLockTimeout) {
		return nil, readHolder(path + ".json"), err
	}
	if err != nil {
		return nil, nil, err
	}
	if holder.Since.IsZero() {
		holder.Since = time.Now().UTC()
	}
	if holder.PID == 0 {
		holder.PID = os.Getpid()
	}
	_ = writeJSONAtomic(path+".json", holder)
	return func() {
		_ = os.Remove(path + ".json")
		unlock()
	}, nil, nil
}

func readHolder(path string) *LockHolder {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var h LockHolder
	if json.Unmarshal(b, &h) != nil {
		return nil
	}
	return &h
}

// HostBuildHolder reports the perflab build holding the host build lock,
// or nil when no build runs. doctor and the measuring verbs read it.
func HostBuildHolder(d Dirs) (*LockHolder, error) {
	if err := os.MkdirAll(d.locksDir(), 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(d.locksDir(), hostBuildLock)
	_, unlock, err := lockFile(path, 0, true)
	if errors.Is(err, errLockTimeout) {
		if h := readHolder(path + ".json"); h != nil {
			return h, nil
		}
		return &LockHolder{Verb: "build native"}, nil
	}
	if err != nil {
		return nil, err
	}
	unlock()
	return nil, nil
}

// ErrRunnerBusy: another run from the same project checkout held its runner
// for the whole wait.
var ErrRunnerBusy = errors.New("the project's runner is busy")

// AcquireProjectRunner serialises `run`s from one project checkout. The
// adapter's runner owns per-checkout resources (FixIt's Appium server port
// is the worktree's slot), so a second phone's run from the same checkout
// waits for the first instead of dying EADDRINUSE in its first case. On a
// timeout it returns the holder (best effort) and ErrRunnerBusy.
func AcquireProjectRunner(d Dirs, projectDir string, wait time.Duration, holder LockHolder) (func(), *LockHolder, error) {
	sum := sha256.Sum256([]byte(filepath.Clean(projectDir)))
	unlock, current, err := d.takeExclusive("runner-"+hex.EncodeToString(sum[:6])+".lock", wait, holder)
	if errors.Is(err, errLockTimeout) {
		return nil, current, ErrRunnerBusy
	}
	return unlock, nil, err
}

// AcquireMeasure marks a measuring window (run, probe) for its whole
// duration: it takes the shared measure lock and then refuses with
// HOST_BUSY_BUILDING while a native build holds the host build lock.
func AcquireMeasure(d Dirs, holder LockHolder) (func(), error) {
	dir := filepath.Join(d.locksDir(), measureHolders)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	_, unlock, err := lockFile(filepath.Join(d.locksDir(), hostMeasureLock), 5*time.Second, true)
	if err != nil {
		return nil, fmt.Errorf("take the measure lock: %w", err)
	}
	if holder.PID == 0 {
		holder.PID = os.Getpid()
	}
	if holder.Since.IsZero() {
		holder.Since = time.Now().UTC()
	}
	record := filepath.Join(dir, fmt.Sprintf("%d.json", holder.PID))
	_ = writeJSONAtomic(record, holder)
	release := func() {
		_ = os.Remove(record)
		unlock()
	}
	builder, err := HostBuildHolder(d)
	if err != nil {
		release()
		return nil, err
	}
	if builder != nil {
		release()
		return nil, diag(DiagHostBusyBuilding,
			"a native build runs on this Mac ("+builder.String()+"); measuring now would read a loaded host",
			"perflab build list --json")
	}
	return release, nil
}

// measuringHolders lists the live measure records when the measure lock is
// held by someone; nil when nobody measures.
func (d Dirs) measuringHolders() ([]LockHolder, bool, error) {
	if err := os.MkdirAll(d.locksDir(), 0o755); err != nil {
		return nil, false, err
	}
	_, unlock, err := lockFile(filepath.Join(d.locksDir(), hostMeasureLock), 0, false)
	if err == nil {
		unlock()
		return nil, false, nil
	}
	if !errors.Is(err, errLockTimeout) {
		return nil, false, err
	}
	entries, _ := os.ReadDir(filepath.Join(d.locksDir(), measureHolders))
	var out []LockHolder
	for _, e := range entries {
		if h := readHolder(filepath.Join(d.locksDir(), measureHolders, e.Name())); h != nil && hostexec.ProcessAlive(h.PID) {
			out = append(out, *h)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.Before(out[j].Since) })
	return out, true, nil
}

// AcquireHostBuild is acquireHostBuild for a build this package does not
// run (`perflab wda build`): the same Mac-wide lock and measure check.
func AcquireHostBuild(d Dirs, wait time.Duration, holder LockHolder, retry string) (func(), error) {
	return d.acquireHostBuild(wait, holder, retry)
}

// acquireHostBuild takes the host build lock (waiting up to wait) and then
// refuses while a run or probe measures. The caller holds the key lock.
func (d Dirs) acquireHostBuild(wait time.Duration, holder LockHolder, retry string) (func(), error) {
	unlock, current, err := d.takeExclusive(hostBuildLock, wait, holder)
	if errors.Is(err, errLockTimeout) {
		who := "another perflab build"
		if current != nil {
			who = current.String()
		}
		return nil, diag(DiagHostBusyBuilding,
			"the Mac-wide build lock is held ("+who+"); builds run one at a time",
			retry+" --wait 60m")
	}
	if err != nil {
		return nil, err
	}
	measuring, busy, err := d.measuringHolders()
	if err != nil {
		unlock()
		return nil, err
	}
	if busy {
		who := "a perflab run"
		if len(measuring) > 0 {
			who = measuring[0].String()
		}
		unlock()
		return nil, diag(DiagHostBusyBuilding,
			"a measurement window is open on this Mac ("+who+"); a build now would skew its frames",
			"perflab lease status --json (wait for the run to end), then "+retry)
	}
	return unlock, nil
}

// writeJSONAtomic writes v as indented JSON via temp file, fsync, rename.
func writeJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(b, '\n'))
}

func writeFileAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

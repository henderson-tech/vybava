package uiloop

// lease.go owns the pass leases, so N identical review-loop runs can share
// one pass: one capture at a time, a reviewer batch claimed by one run, one
// synthesizer and one publisher. A lease is <passDir>/locks/<name>.json
// (capture, synth, publish, batch-<id>), created exclusively. It holds until
// its holder releases it or it goes stale (leaseStale), and a stale lease is
// replaced by the next taker. There are two kinds:
//
//   - A process lease (pid > 0: capture, publish, a merge-review without
//     --owner) is held by one vybava process and released when it exits,
//     error or not. Only a crash leaves one behind, and its dead pid stales it.
//   - An owner lease (pid 0: a batch claim, a merge-review's synth with
//     --owner) is held for a workflow run past the verb that took it, until
//     its ttl runs out; the same owner takes it back (renews it) any time.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/henderson-tech/vybava/internal/runx"
)

// Lease names; a batch claim is batchLease(id).
const (
	leaseCapture = "capture"
	leaseSynth   = "synth"
	leasePublish = "publish"
)

func batchLease(id string) string { return "batch-" + id }

const (
	// captureLeaseTTL bounds a capture lease whose pid this host cannot judge
	// (taken on another host, or a pid reused after a reboot); on its own host
	// the pid is its liveness and done.json its end. A pass took 10–25 min.
	captureLeaseTTL = 6 * time.Hour
	// DefaultLeaseTTL is synth's and publish's ttl, DefaultClaimTTL a batch claim's.
	DefaultLeaseTTL = time.Hour
	DefaultClaimTTL = 3 * time.Hour
)

// Lease is <passDir>/locks/<name>.json. PID is 0 for an owner lease; Run is
// the createdAt of the run.json a capture lease was taken for, else empty.
type Lease struct {
	Owner     string `json:"owner"`
	Host      string `json:"host"`
	PID       int    `json:"pid"`
	StartedAt string `json:"startedAt"` // RFC3339
	TTL       string `json:"ttl"`       // a Go duration
	Run       string `json:"run"`
}

// sameHolder: the same owner, host and process (pid 0 for an owner lease,
// which every process of that owner takes back).
func (l Lease) sameHolder(o Lease) bool {
	return l.Owner == o.Owner && l.Host == o.Host && l.PID == o.PID
}

// where names the holder for a diagnostic.
func (l Lease) where() string {
	if l.PID > 0 {
		return fmt.Sprintf("pid %d on %s, ttl %s", l.PID, l.Host, l.TTL)
	}
	return fmt.Sprintf("on %s, ttl %s", l.Host, l.TTL)
}

// heldLease is a lease file as read back. File is repo-relative; Stale says
// why it no longer holds, "" while it does.
type heldLease struct {
	Lease
	Name, File string
	Stale      string
}

// leaseReq is a lease to take: pid 0 holds it for owner past this process.
type leaseReq struct {
	owner string
	pid   int
	ttl   time.Duration
	run   string
}

// processLease is held by this process for owner (the verb's own name
// when no --owner is given) until it exits.
func processLease(owner, verb string, ttl time.Duration) leaseReq {
	if owner == "" {
		owner = "ui-loop " + verb
	}
	return leaseReq{owner: owner, pid: os.Getpid(), ttl: ttl}
}

var leaseHost = sync.OnceValues(os.Hostname)

func (t *Tool) leaseFile(pass int, name string) string {
	return filepath.Join(t.passAbs(pass), "locks", name+".json")
}

// readLease reads and judges name's lease on pass; nil when there is none. A
// file that does not decode names no holder, so it is stale.
func (t *Tool) readLease(pass int, name string) (*heldLease, error) {
	b, err := os.ReadFile(t.leaseFile(pass, name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	h := &heldLease{Name: name, File: path.Join(t.PassDir(pass), "locks", name+".json")}
	if err := json.Unmarshal(b, &h.Lease); err != nil {
		h.Stale = "it does not decode: " + err.Error()
		return h, nil
	}
	h.Stale, err = t.leaseStale(pass, h.Lease)
	return h, err
}

// leaseStale says why l no longer holds, "" while it does: its pid is gone
// (on this host, the only one whose pids it can see), its ttl ran out, or
// the run it was taken for wrote the pass's done.json.
func (t *Tool) leaseStale(pass int, l Lease) (string, error) {
	started, err := time.Parse(time.RFC3339, l.StartedAt)
	if err != nil {
		return fmt.Sprintf("its startedAt %q is no RFC3339 time", l.StartedAt), nil
	}
	ttl, err := time.ParseDuration(l.TTL)
	if err != nil {
		return fmt.Sprintf("its ttl %q is no duration", l.TTL), nil
	}
	host, err := leaseHost()
	if err != nil {
		return "", err
	}
	if l.PID > 0 && l.Host == host && !pidAlive(l.PID) {
		return fmt.Sprintf("pid %d on %s is gone", l.PID, host), nil
	}
	if end := started.Add(ttl); !t.Now().Before(end) {
		return "its ttl ran out at " + end.UTC().Format(time.RFC3339), nil
	}
	if l.Run != "" {
		var done DoneFile
		found, err := readJSON(filepath.Join(t.passAbs(pass), "done.json"), &done)
		if err != nil {
			return "", err
		}
		if found && done.Run == l.Run {
			return "its run finished (done.json)", nil
		}
	}
	return "", nil
}

// passLeases reads every lease of the pass, by name.
func (t *Tool) passLeases(pass int) ([]heldLease, error) {
	files, err := filepath.Glob(filepath.Join(t.passAbs(pass), "locks", "*.json"))
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	var out []heldLease
	for _, f := range files {
		h, err := t.readLease(pass, strings.TrimSuffix(filepath.Base(f), ".json"))
		if err != nil {
			return nil, err
		}
		if h != nil {
			out = append(out, *h)
		}
	}
	return out, nil
}

// liveCapture is the capture lease that holds, the newest pass first: one
// capture runs under <out> at a time, whichever pass it shoots.
func (t *Tool) liveCapture() (int, *heldLease, error) {
	passes, err := t.Passes()
	if err != nil {
		return 0, nil, err
	}
	for i := len(passes) - 1; i >= 0; i-- {
		h, err := t.readLease(passes[i], leaseCapture)
		if err != nil {
			return 0, nil, err
		}
		if h != nil && h.Stale == "" {
			return passes[i], h, nil
		}
	}
	return 0, nil, nil
}

// underLeases runs fn holding the pass's lease mutex (an flock on
// locks/.mutex, which the kernel drops with its process): every take and
// release judges and replaces a lease under it, so a stale lease is
// replaced once and two claims never take one batch. Readers need no
// mutex, because a lease file appears whole (createLease).
func (t *Tool) underLeases(pass int, fn func() error) error {
	dir := filepath.Join(t.passAbs(pass), "locks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	unlock, err := lockLeases(filepath.Join(dir, ".mutex"))
	if err != nil {
		return err
	}
	err = fn()
	if uerr := unlock(); err == nil {
		err = uerr
	}
	return err
}

// createLease creates file exclusively, as O_CREATE|O_EXCL does, but whole:
// a hard link of a written temp file either lands or finds the name taken
// (fs.ErrExist), so no reader ever decodes a half-written lease.
func createLease(file string, l Lease) error {
	b, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(file), ".lease-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	_, err = tmp.Write(append(b, '\n'))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Link(tmp.Name(), file)
}

// takeLease takes name for r; the caller holds the mutex. An absent or
// stale lease is replaced, r's own live lease is renewed (its startedAt
// kept, its ttl counted from now), and another holder's live lease comes
// back as held.
func (t *Tool) takeLease(pass int, name string, r leaseReq) (Lease, *heldLease, error) {
	host, err := leaseHost()
	if err != nil {
		return Lease{}, nil, err
	}
	now := t.Now().UTC()
	l := Lease{Owner: r.owner, Host: host, PID: r.pid, StartedAt: now.Format(time.RFC3339), TTL: r.ttl.String(), Run: r.run}
	cur, err := t.readLease(pass, name)
	if err != nil {
		return Lease{}, nil, err
	}
	file := t.leaseFile(pass, name)
	if cur != nil {
		if cur.Stale == "" {
			if !cur.sameHolder(l) {
				return Lease{}, cur, nil
			}
			started, err := time.Parse(time.RFC3339, cur.StartedAt)
			if err != nil {
				return Lease{}, nil, err
			}
			l.StartedAt, l.TTL = cur.StartedAt, (now.Sub(started) + r.ttl).Round(time.Second).String()
			// One rename replaces it: a reader never finds a held lease gone.
			return l, nil, writeJSON(file, l)
		}
		if err := os.Remove(file); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return Lease{}, nil, err
		}
	}
	if err := createLease(file, l); errors.Is(err, fs.ErrExist) {
		// Taken outside the mutex: a lost race is held, like any other.
		held, rerr := t.readLease(pass, name)
		if rerr != nil || held == nil {
			return Lease{}, nil, errors.Join(err, rerr)
		}
		return Lease{}, held, nil
	} else if err != nil {
		return Lease{}, nil, err
	}
	return l, nil, nil
}

// acquireLease takes name on pass for r (takeLease) under the mutex.
func (t *Tool) acquireLease(pass int, name string, r leaseReq) (l Lease, held *heldLease, err error) {
	err = t.underLeases(pass, func() error {
		l, held, err = t.takeLease(pass, name, r)
		return err
	})
	return l, held, err
}

// releaseLease removes name's lease while it is still l: one replaced
// after it went stale is its new holder's.
func (t *Tool) releaseLease(pass int, name string, l Lease) error {
	return t.underLeases(pass, func() error {
		cur, err := t.readLease(pass, name)
		if err != nil || cur == nil || !cur.sameHolder(l) || cur.StartedAt != l.StartedAt {
			return err
		}
		return os.Remove(t.leaseFile(pass, name))
	})
}

// dropLease releases a process lease on the way out of its verb (defer it);
// a failure joins the verb's own error.
func (t *Tool) dropLease(pass int, name string, l Lease, err *error) {
	if rerr := t.releaseLease(pass, name, l); rerr != nil {
		*err = errors.Join(*err, fmt.Errorf("releasing %s/locks/%s.json: %w", t.PassDir(pass), name, rerr))
	}
}

// claimBatches claims up to n of the left batch ids for owner, in order, as
// batch-<id> owner leases: the ones owner already holds first (renewed),
// then unclaimed or stale-claimed ones; another owner's live claim is
// skipped. Under one mutex, so two claims never share a batch.
func (t *Tool) claimBatches(pass int, left []string, n int, owner string, ttl time.Duration) ([]string, error) {
	r := leaseReq{owner: owner, ttl: ttl}
	host, err := leaseHost()
	if err != nil {
		return nil, err
	}
	me := Lease{Owner: owner, Host: host}
	claimed := []string{}
	err = t.underLeases(pass, func() error {
		var mine, free []string
		for _, id := range left {
			cur, err := t.readLease(pass, batchLease(id))
			if err != nil {
				return err
			}
			switch {
			case cur == nil || cur.Stale != "":
				free = append(free, id)
			case cur.sameHolder(me):
				mine = append(mine, id)
			}
		}
		for _, id := range append(mine, free...) {
			if len(claimed) == n {
				break
			}
			_, held, err := t.takeLease(pass, batchLease(id), r)
			if err != nil {
				return err
			}
			if held == nil {
				claimed = append(claimed, id)
			}
		}
		return nil
	})
	return claimed, err
}

// captureRunning refuses a second capture while h holds pass's.
func captureRunning(pass int, h *heldLease) runx.DiagError {
	return diag(DiagCaptureRunning, fmt.Sprintf("pass %d capture running since %s (%s, %s)", pass, h.StartedAt, h.Owner, h.where()),
		"vybava ui-loop state --json")
}

// leaseHeld refuses a verb whose lease h holds.
func leaseHeld(pass int, h *heldLease) runx.DiagError {
	return diag(DiagLeaseHeld, fmt.Sprintf("%s of pass %d is held by %s since %s (%s)", h.Name, pass, h.Owner, h.StartedAt, h.where()),
		fmt.Sprintf("leave it to %s; remove %s only once that holder is gone", h.Owner, h.File))
}

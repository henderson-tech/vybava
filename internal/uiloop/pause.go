package uiloop

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// Pause is <out>/paused.json, written by `ui-loop pause`: the whole polish
// loop drains. Nothing new starts (run, batches --claim and lanes refuse;
// state routes `paused`), a running stage ends at its next phase boundary
// and a running capture finishes. Pass is the newest pass with shots when it
// paused, for the record. A pause writes nothing else: claims, checkpoints,
// recovery files and leases stay as they are.
type Pause struct {
	By     string `json:"by"`
	At     string `json:"at"` // RFC3339
	Reason string `json:"reason"`
	Pass   int    `json:"pass"`
}

func (p Pause) why() string {
	why := fmt.Sprintf("paused by %s at %s", p.By, p.At)
	if p.Reason != "" {
		why += ": " + p.Reason
	}
	return why
}

const resumeCommand = "vybava ui-loop resume"

func (t *Tool) pauseFile() string { return t.abs(filepath.Join(t.Config.Out, "paused.json")) }

// paused is the loop's pause, nil when it runs.
func (t *Tool) paused() (*Pause, error) {
	var p Pause
	found, err := readJSON(t.pauseFile(), &p)
	if err != nil || !found {
		return nil, err
	}
	return &p, nil
}

// refusePaused is the PAUSED refusal of a verb that starts work.
func (t *Tool) refusePaused(verb string) error {
	p, err := t.paused()
	if err != nil || p == nil {
		return err
	}
	return diag(DiagPaused, fmt.Sprintf("the polish loop is %s, so %s starts nothing", p.why(), verb), resumeCommand)
}

// PauseOptions are `pause`'s flags.
type PauseOptions struct {
	Reason string
	// By names who paused (default user@host).
	By string
}

// PauseData is `pause`'s and `resume`'s data. Released lists the owner
// leases resume freed, as <passDir>/locks/<name>.json.
type PauseData struct {
	Paused   *Pause   `json:"paused"`
	Changed  bool     `json:"changed"`
	Released []string `json:"released,omitempty"`
}

// underPause runs fn holding <out>/.pause.lock (an flock the kernel drops
// with its process): pause and resume each read and write paused.json under
// it, so of two pauses the first stands and a resume never removes a pause
// taken while it ran.
func (t *Tool) underPause(fn func() error) error {
	dir := t.abs(t.Config.Out)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	unlock, err := lockLeases(filepath.Join(dir, ".pause.lock"))
	if err != nil {
		return err
	}
	err = fn()
	if uerr := unlock(); err == nil {
		err = uerr
	}
	return err
}

// Pause writes paused.json, or leaves the pause already there as it is.
func (t *Tool) Pause(o PauseOptions) (Result, error) {
	var data PauseData
	err := t.underPause(func() error {
		p, err := t.paused()
		if err != nil || p != nil {
			data.Paused = p
			return err
		}
		pass, _, err := t.newestShotPass()
		if err != nil {
			return err
		}
		by := o.By
		if by == "" {
			host, err := leaseHost()
			if err != nil {
				return err
			}
			by = os.Getenv("USER") + "@" + host
		}
		data = PauseData{Paused: &Pause{By: by, At: t.Now().UTC().Format(time.RFC3339), Reason: o.Reason, Pass: pass}, Changed: true}
		return writeJSON(t.pauseFile(), data.Paused)
	})
	if err != nil {
		return Result{}, err
	}
	return Result{Data: data, Next: []string{resumeCommand}}, nil
}

// Resume removes paused.json and frees every pass's owner leases (batch
// claims, the synth lease): the runs that held them drained during the
// pause, and the next run takes their batches at once. Without a pause it
// does nothing, so a live run's claims are never freed.
func (t *Tool) Resume() (Result, error) {
	next := []string{"vybava ui-loop state --json"}
	// Without a pause there is nothing to lock either.
	if p, err := t.paused(); err != nil || p == nil {
		return Result{Data: PauseData{}, Next: next}, err
	}
	var data PauseData
	err := t.underPause(func() error {
		p, err := t.paused()
		if err != nil || p == nil {
			return err
		}
		if data, err = t.freeOwnerLeases(); err != nil {
			return err
		}
		if err := os.Remove(t.pauseFile()); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		data.Changed = true
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	return Result{Data: data, Next: next}, nil
}

// freeOwnerLeases removes every pass's owner leases (pid 0), each pass under
// its lease mutex; a pass without one is not locked, so no locks/ appears.
func (t *Tool) freeOwnerLeases() (PauseData, error) {
	passes, err := t.Passes()
	if err != nil {
		return PauseData{}, err
	}
	var data PauseData
	owned := func(h heldLease) bool { return h.PID == 0 }
	for _, pass := range passes {
		// Readers need no mutex.
		held, err := t.passLeases(pass)
		if err != nil {
			return PauseData{}, err
		}
		if !slices.ContainsFunc(held, owned) {
			continue
		}
		err = t.underLeases(pass, func() error {
			held, err := t.passLeases(pass)
			if err != nil {
				return err
			}
			for _, h := range held {
				if !owned(h) {
					continue
				}
				if err := os.Remove(t.leaseFile(pass, h.Name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
					return err
				}
				data.Released = append(data.Released, h.File)
			}
			return nil
		})
		if err != nil {
			return PauseData{}, err
		}
	}
	return data, nil
}

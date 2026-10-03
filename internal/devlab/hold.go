package devlab

import (
	"context"
	"fmt"
	"time"
)

// Hold is one device-touching verb's grip on a device: the token verified,
// the device lock held, the heartbeat bumped. Every verb that runs adb or
// devicectl against a leased device (install, app, net, run, probe,
// capture, crashes, the device passthroughs) goes through Lab.Hold.
type Hold struct {
	ID     string
	Device *Device
	Token  string
	// Generation is the lease generation this hold verified.
	Generation int
	lab        *Lab
	release    func()
}

// Hold verifies the token (LEASE_REQUIRED / LEASE_INVALID), takes the
// device lock (DEVICE_BUSY) and bumps the heartbeat. Callers defer Done.
func (l *Lab) Hold(handle, token, verb string) (*Hold, error) {
	id, dev, ls, err := l.Verify(handle, token)
	if err != nil {
		return nil, err
	}
	release, err := l.LockDevice(id, verb)
	if err != nil {
		return nil, err
	}
	h := &Hold{ID: id, Device: dev, Token: token, Generation: ls.Generation, lab: l, release: release}
	if err := h.Heartbeat(); err != nil {
		release()
		return nil, err
	}
	return h, nil
}

// Done releases the device lock (the lease stays).
func (h *Hold) Done() {
	if h.release != nil {
		h.release()
		h.release = nil
	}
}

// update re-reads the lease under its lock and refuses when the token no
// longer holds it (broken or reaped mid-verb).
func (h *Hold) update(verb string, fn func(*Lease)) error {
	_, err := h.lab.updateLease(h.ID, verb, func(ls *Lease) error {
		if err := h.lab.checkToken(ls, h.Token, false); err != nil {
			return err
		}
		fn(ls)
		return nil
	})
	return err
}

// Heartbeat bumps heartbeatAt.
func (h *Hold) Heartbeat() error {
	return h.update("heartbeat", func(ls *Lease) {
		now := h.lab.now()
		ls.HeartbeatAt = &now
	})
}

// HeartbeatEvery is how often a long verb bumps the heartbeat.
const HeartbeatEvery = 60 * time.Second

// ActiveGrace is how far ahead a running long verb keeps its lease. A run
// started near the end of the TTL would otherwise lose the lease mid-block:
// its next install fails LEASE_INVALID, and an expired lease can be broken
// under a live measurement. Only a verb still beating extends it, so a
// holder that stopped (or died) is bounded by the TTL plus this grace.
const ActiveGrace = 5 * time.Minute

// keepAlive bumps the heartbeat and keeps expiresAt at least ActiveGrace
// ahead.
func (h *Hold) keepAlive() error {
	return h.update("heartbeat", func(ls *Lease) {
		now := h.lab.now()
		ls.HeartbeatAt = &now
		if floor := now.Add(ActiveGrace); ls.ExpiresAt == nil || ls.ExpiresAt.Before(floor) {
			ls.ExpiresAt = &floor
		}
	})
}

// StartHeartbeat keeps the lease alive (keepAlive) now and every interval
// until stop is called or ctx ends; a failed bump (the lease was broken) is
// reported on errs.
func (h *Hold) StartHeartbeat(ctx context.Context, every time.Duration) (stop func(), errs <-chan error) {
	ctx, cancel := context.WithCancel(ctx)
	ch := make(chan error, 1)
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		if err := h.keepAlive(); err != nil {
			ch <- err
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := h.keepAlive(); err != nil {
					select {
					case ch <- err:
					default:
					}
					return
				}
			}
		}
	}()
	return cancel, ch
}

// RecordInstall writes the fence: what was installed, under this hold's
// generation.
func (h *Hold) RecordInstall(inst Installed) error {
	return h.update("install", func(ls *Lease) {
		inst.At = h.lab.now()
		inst.ByGeneration = ls.Generation
		ls.LastInstalled = &inst
	})
}

// CheckInstalled compares what is on the device now with the fence:
// Android by the base APK's sha256, iOS by the bundle version stamp. Any
// difference, or nothing recorded, is DEVICE_STATE_CHANGED.
func (h *Hold) CheckInstalled(observed Installed) error {
	ls, err := h.lab.readLease(h.ID)
	if err != nil {
		return err
	}
	return checkFence(h.ID, h.Token, ls.LastInstalled, observed)
}

func checkFence(id, token string, last *Installed, observed Installed) error {
	if last == nil {
		return diag(DiagDeviceStateChanged, fmt.Sprintf("perflab has no install recorded on %s, so what runs there is unknown", id),
			fmt.Sprintf("perflab install <variant> --device %s --lease %s --json", id, token))
	}
	fix := fmt.Sprintf("perflab install %s --device %s --lease %s --json", last.VariantID, id, token)
	if last.Package != "" && observed.Package != "" && last.Package != observed.Package {
		return diag(DiagDeviceStateChanged, fmt.Sprintf("%s: perflab installed %s, the check read %s", id, last.Package, observed.Package), fix)
	}
	switch {
	case last.ArtifactSHA256 != "" && observed.ArtifactSHA256 != "":
		if last.ArtifactSHA256 != observed.ArtifactSHA256 {
			return diag(DiagDeviceStateChanged, fmt.Sprintf("%s: the installed APK (sha256 %s) is not %s (sha256 %s), installed by generation %d", id, short(observed.ArtifactSHA256), last.VariantID, short(last.ArtifactSHA256), last.ByGeneration), fix)
		}
	case last.BundleVersion != "" && observed.BundleVersion != "":
		if last.BundleVersion != observed.BundleVersion {
			return diag(DiagDeviceStateChanged, fmt.Sprintf("%s: the installed bundle version %s is not %s's %s, installed by generation %d", id, observed.BundleVersion, last.VariantID, last.BundleVersion, last.ByGeneration), fix)
		}
	default:
		return diag(DiagDeviceStateChanged, fmt.Sprintf("%s: the fence has no comparable fingerprint (need an APK sha256 or a bundle version on both sides)", id), fix)
	}
	return nil
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// RecordChild records a process group this lease owns, so a release (or
// the SessionEnd hook) stops it.
func (h *Hold) RecordChild(c Child) error {
	return h.update("record child", func(ls *Lease) {
		if c.PGID == 0 {
			c.PGID = c.PID
		}
		if c.StartedAt.IsZero() {
			if start, ok, err := h.lab.ProcStart(c.PID); err == nil && ok {
				c.StartedAt = start
			}
		}
		ls.Children = append(ls.Children, c)
	})
}

// ForgetChild drops a child that exited on its own.
func (h *Hold) ForgetChild(pid int) error {
	return h.update("forget child", func(ls *Lease) {
		kept := ls.Children[:0]
		for _, c := range ls.Children {
			if c.PID != pid {
				kept = append(kept, c)
			}
		}
		ls.Children = kept
	})
}

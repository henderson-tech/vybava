package devlab

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/runx"
)

// Lease TTLs (docs/perflab.md "Concurrency, fencing and staleness").
const (
	DefaultLeaseTTL = 2 * time.Hour
	MaxLeaseTTL     = 8 * time.Hour
)

// Lease is leases/<deviceId>.json. The file outlives a release: the
// generation (the fencing counter) and lastInstalled carry over to the next
// holder. Held means TokenSHA256 is set.
type Lease struct {
	DeviceID    string     `json:"deviceId"`
	TokenSHA256 string     `json:"tokenSha256,omitempty"`
	Generation  int        `json:"generation"`
	Owner       *Owner     `json:"owner,omitempty"`
	Purpose     string     `json:"purpose,omitempty"`
	AcquiredAt  *time.Time `json:"acquiredAt,omitempty"`
	HeartbeatAt *time.Time `json:"heartbeatAt,omitempty"`
	ExpiresAt   *time.Time `json:"expiresAt,omitempty"`
	// LastInstalled is the fence: what perflab last installed on the
	// device, and under which generation.
	LastInstalled *Installed `json:"lastInstalled,omitempty"`
	// Children are process groups a verb started under this lease
	// (forwarders, perfetto, xctrace); release stops them.
	Children []Child  `json:"children,omitempty"`
	Released *Release `json:"released,omitempty"`
}

// Installed is one install perflab made. Android compares the base APK's
// sha256, iOS the CFBundleVersion stamp `pack` writes as <build>.<sha6>.
type Installed struct {
	VariantID      string    `json:"variantId"`
	Package        string    `json:"package,omitempty"`
	ArtifactSHA256 string    `json:"artifactSha256,omitempty"`
	BundleVersion  string    `json:"bundleVersion,omitempty"`
	At             time.Time `json:"at"`
	ByGeneration   int       `json:"byGeneration"`
}

// Child is a process group a lease owns.
type Child struct {
	PID       int       `json:"pid"`
	PGID      int       `json:"pgid"`
	What      string    `json:"what"`
	StartedAt time.Time `json:"startedAt"`
}

// Release records how the last lease ended.
type Release struct {
	At     time.Time `json:"at"`
	Reason string    `json:"reason"`
	Holder string    `json:"holder,omitempty"`
}

// Held reports whether a token currently holds the device.
func (ls *Lease) Held() bool { return ls != nil && ls.TokenSHA256 != "" }

func (ls *Lease) expired(now time.Time) bool {
	return ls.ExpiresAt != nil && !now.Before(*ls.ExpiresAt)
}

// LeaseView is the sanitized lease every verb prints: the token's hash
// prefix only, never the token.
type LeaseView struct {
	Device        string     `json:"device"`
	Held          bool       `json:"held"`
	Generation    int        `json:"generation"`
	TokenHash     string     `json:"tokenHash,omitempty"`
	Holder        *Owner     `json:"holder,omitempty"`
	HolderAlive   Liveness   `json:"holderAlive,omitempty"`
	Purpose       string     `json:"purpose,omitempty"`
	AcquiredAt    *time.Time `json:"acquiredAt,omitempty"`
	HeartbeatAt   *time.Time `json:"heartbeatAt,omitempty"`
	ExpiresAt     *time.Time `json:"expiresAt,omitempty"`
	HeldFor       string     `json:"heldFor,omitempty"`
	Expired       bool       `json:"expired,omitempty"`
	Stale         bool       `json:"stale,omitempty"`
	LastInstalled *Installed `json:"lastInstalled,omitempty"`
	Children      []Child    `json:"children,omitempty"`
	Released      *Release   `json:"released,omitempty"`
}

func (l *Lab) view(ls *Lease) LeaseView {
	v := LeaseView{Device: ls.DeviceID, Held: ls.Held(), Generation: ls.Generation, LastInstalled: ls.LastInstalled, Released: ls.Released}
	if !ls.Held() {
		return v
	}
	now := l.now()
	v.TokenHash = ls.TokenSHA256[:8]
	v.Holder, v.Purpose = ls.Owner, ls.Purpose
	v.AcquiredAt, v.HeartbeatAt, v.ExpiresAt = ls.AcquiredAt, ls.HeartbeatAt, ls.ExpiresAt
	v.HolderAlive = l.liveness(ls.Owner)
	v.Expired = ls.expired(now)
	v.Stale = v.Expired && v.HolderAlive != Alive
	if ls.AcquiredAt != nil {
		v.HeldFor = now.Sub(*ls.AcquiredAt).Round(time.Second).String()
	}
	v.Children = ls.Children
	v.Released = nil
	return v
}

// stale: expired AND the holder is not provably alive. A dead holder within
// its TTL is not stale: a session can restart claude and resume with the
// token.
func (l *Lab) stale(ls *Lease) bool {
	return ls.Held() && ls.expired(l.now()) && l.liveness(ls.Owner) != Alive
}

func (l *Lab) readLease(id string) (*Lease, error) {
	b, err := os.ReadFile(l.leasePath(id))
	if errors.Is(err, os.ErrNotExist) {
		return &Lease{DeviceID: id}, nil
	}
	if err != nil {
		return nil, err
	}
	var ls Lease
	if err := json.Unmarshal(b, &ls); err != nil {
		return nil, fmt.Errorf("parse %s: %w", l.leasePath(id), err)
	}
	ls.DeviceID = id
	return &ls, nil
}

// updateLease is the one read-modify-write path of a lease file.
func (l *Lab) updateLease(id, verb string, fn func(*Lease) error) (*Lease, error) {
	h, err := l.lockLease(id, verb)
	if err != nil {
		return nil, err
	}
	defer h.Release()
	ls, err := l.readLease(id)
	if err != nil {
		return nil, err
	}
	if err := fn(ls); err != nil {
		return ls, err
	}
	return ls, writeJSONAtomic(l.leasePath(id), ls)
}

// newToken mints the capability: 160 random bits, base32, `plt_` prefixed so
// a reader recognises it in a command line.
func newToken() (string, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "plt_" + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b)), nil
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// AcquireOptions are `lease acquire`'s flags.
type AcquireOptions struct {
	For     time.Duration
	Purpose string
	Wait    time.Duration
}

// AcquireData is `lease acquire`'s payload: the token, printed this once.
type AcquireData struct {
	Device string    `json:"device"`
	Token  string    `json:"token"`
	Lease  LeaseView `json:"lease"`
}

// Acquire mints a token and writes the lease. A live lease held by anyone
// (this session included: two agent copies share it) answers DEVICE_LEASED
// naming the holder; an expired lease of a dead holder is reclaimed with
// LEASE_STALE_RECLAIMED.
func (l *Lab) Acquire(ctx context.Context, handle string, opts AcquireOptions) (Result, error) {
	ttl, err := leaseTTL(opts.For)
	if err != nil {
		return Result{}, err
	}
	id, _, err := l.resolve(handle)
	if err != nil {
		return Result{}, err
	}
	owner := l.currentOwner(ctx)
	deadline := time.Now().Add(opts.Wait) // wall clock: the wait is real time
	for {
		res, err := l.tryAcquire(id, ttl, opts.Purpose, owner)
		var de runx.DiagError
		if err == nil || !errors.As(err, &de) || de.Diag.Code != DiagDeviceLeased || !time.Now().Before(deadline) {
			return res, err
		}
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func leaseTTL(d time.Duration) (time.Duration, error) {
	switch {
	case d == 0:
		return DefaultLeaseTTL, nil
	case d < 0 || d > MaxLeaseTTL:
		return 0, usage(fmt.Sprintf("--for %s is outside 1s..%s", d, MaxLeaseTTL), "--for 2h (max 8h; renew with perflab lease renew)")
	}
	return d.Round(time.Second), nil
}

func (l *Lab) tryAcquire(id string, ttl time.Duration, purpose string, owner *Owner) (Result, error) {
	token, err := newToken()
	if err != nil {
		return Result{}, err
	}
	var diags []runx.Diagnostic
	ls, err := l.updateLease(id, "lease acquire", func(ls *Lease) error {
		if ls.Held() {
			if !l.stale(ls) {
				return l.leasedErr(ls)
			}
			diags = append(diags, infoRow(DiagLeaseStaleReclaimed,
				fmt.Sprintf("took over %s's lease on %s: it expired at %s and its holder is gone", ls.Owner, id, ls.ExpiresAt.Format(time.RFC3339)), ""))
			l.stopChildren(ls)
		}
		now := l.now()
		exp := now.Add(ttl)
		ls.TokenSHA256 = tokenHash(token)
		ls.Generation++
		ls.Owner, ls.Purpose = owner, purpose
		ls.AcquiredAt, ls.HeartbeatAt, ls.ExpiresAt = &now, &now, &exp
		ls.Children, ls.Released = nil, nil
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	v := l.view(ls)
	return Result{
		Data:        AcquireData{Device: id, Token: token, Lease: v},
		Lines:       []string{fmt.Sprintf("%s leased until %s (generation %d)", id, ls.ExpiresAt.Local().Format("15:04"), ls.Generation), "token (printed once): " + token},
		Diagnostics: diags,
		Next: []string{
			fmt.Sprintf("perflab device probe %s --lease %s --json", id, token),
			fmt.Sprintf("perflab doctor --device %s --lease %s --json", id, token),
			fmt.Sprintf("perflab lease release %s --lease %s --json", id, token),
		},
	}, nil
}

// leasedErr is DEVICE_LEASED naming the holder, how long it has held, and
// when the lease ends.
func (l *Lab) leasedErr(ls *Lease) error {
	now := l.now()
	detail := fmt.Sprintf("%s is leased by %s", ls.DeviceID, ls.Owner)
	if ls.AcquiredAt != nil {
		detail += fmt.Sprintf(" for %s", now.Sub(*ls.AcquiredAt).Round(time.Second))
	}
	if ls.Purpose != "" {
		detail += fmt.Sprintf(" (%q)", ls.Purpose)
	}
	if ls.ExpiresAt != nil {
		detail += fmt.Sprintf("; it expires at %s", ls.ExpiresAt.Format(time.RFC3339))
	}
	if sid := l.Getenv("CLAUDE_CODE_SESSION_ID"); sid != "" && ls.Owner != nil && ls.Owner.SessionID == sid {
		detail += "; the holder is THIS session (another agent copy, or an earlier acquire): use that token, never a second lease"
	}
	return diag(DiagDeviceLeased, detail, fmt.Sprintf("perflab lease status %s --json", ls.DeviceID))
}

// checkToken verifies a token against a lease. allowExpired lets the holder
// renew or release a lease whose TTL ran out.
func (l *Lab) checkToken(ls *Lease, token string, allowExpired bool) error {
	id := ls.DeviceID
	if strings.TrimSpace(token) == "" {
		return diag(DiagLeaseRequired, fmt.Sprintf("%s is only touched under a lease", id), fmt.Sprintf("perflab lease acquire %s --json", id))
	}
	if !ls.Held() {
		return diag(DiagLeaseInvalid, fmt.Sprintf("%s is not leased (the lease was released, reaped or broken)", id), fmt.Sprintf("perflab lease acquire %s --json", id))
	}
	if tokenHash(token) != ls.TokenSHA256 {
		return diag(DiagLeaseInvalid, fmt.Sprintf("the token does not hold %s; it is leased by %s", id, ls.Owner), fmt.Sprintf("perflab lease status %s --json", id))
	}
	if !allowExpired && ls.expired(l.now()) {
		return diag(DiagLeaseInvalid, fmt.Sprintf("the lease on %s expired at %s", id, ls.ExpiresAt.Format(time.RFC3339)), fmt.Sprintf("perflab lease renew %s --lease %s --for 2h --json", id, token))
	}
	return nil
}

// Verify checks a token without touching the device: LEASE_REQUIRED,
// LEASE_INVALID or nil.
func (l *Lab) Verify(handle, token string) (string, *Device, *Lease, error) {
	id, dev, err := l.resolve(handle)
	if err != nil {
		return "", nil, nil, err
	}
	ls, err := l.readLease(id)
	if err != nil {
		return "", nil, nil, err
	}
	if err := l.checkToken(ls, token, false); err != nil {
		return "", nil, nil, err
	}
	return id, dev, ls, nil
}

// RenewOptions are `lease renew`'s flags.
type RenewOptions struct{ For time.Duration }

// Renew extends the expiry from now; an expired lease the token still holds
// renews too.
func (l *Lab) Renew(handle, token string, opts RenewOptions) (Result, error) {
	ttl, err := leaseTTL(opts.For)
	if err != nil {
		return Result{}, err
	}
	id, _, err := l.resolve(handle)
	if err != nil {
		return Result{}, err
	}
	ls, err := l.updateLease(id, "lease renew", func(ls *Lease) error {
		if err := l.checkToken(ls, token, true); err != nil {
			return err
		}
		now := l.now()
		exp := now.Add(ttl)
		ls.HeartbeatAt, ls.ExpiresAt = &now, &exp
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	return Result{
		Data:  l.view(ls),
		Lines: []string{fmt.Sprintf("%s renewed until %s", id, ls.ExpiresAt.Local().Format("15:04"))},
		Next:  []string{fmt.Sprintf("perflab lease release %s --lease %s --json", id, token)},
	}, nil
}

// ReleaseLease ends the token's lease and stops the process groups it
// recorded. A wrong token answers LEASE_INVALID.
func (l *Lab) ReleaseLease(handle, token string) (Result, error) {
	id, _, err := l.resolve(handle)
	if err != nil {
		return Result{}, err
	}
	ls, err := l.updateLease(id, "lease release", func(ls *Lease) error {
		if err := l.checkToken(ls, token, true); err != nil {
			return err
		}
		l.end(ls, "released by its holder")
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	return Result{Data: l.view(ls), Lines: []string{id + " released"}, Next: []string{}}, nil
}

// end clears a held lease in place (callers hold its lock), keeping the
// generation and the fence.
func (l *Lab) end(ls *Lease, reason string) {
	l.stopChildren(ls)
	ls.Released = &Release{At: l.now(), Reason: reason, Holder: ls.Owner.String()}
	ls.TokenSHA256, ls.Owner, ls.Purpose = "", nil, ""
	ls.AcquiredAt, ls.HeartbeatAt, ls.ExpiresAt = nil, nil, nil
	ls.Children = nil
}

// stopChildren SIGTERMs each recorded process group whose leader is still
// the process the lease recorded (same start time); a recycled pid or an
// unreadable process table is left alone.
func (l *Lab) stopChildren(ls *Lease) {
	for _, c := range ls.Children {
		start, ok, err := l.ProcStart(c.PID)
		if err != nil || !ok {
			continue
		}
		if d := start.Sub(c.StartedAt); d > startSlack || d < -startSlack {
			continue
		}
		_ = l.StopGroup(c.PGID)
	}
}

// StatusData is `lease status`'s payload.
type StatusData struct {
	Leases []LeaseView `json:"leases"`
}

// Status lists every device's lease (or one device's).
func (l *Lab) Status(handle string) (Result, error) {
	var ids []string
	if strings.TrimSpace(handle) != "" {
		id, _, err := l.resolve(handle)
		if err != nil {
			return Result{}, err
		}
		ids = []string{id}
	} else {
		var err error
		if ids, err = l.leaseIDs(); err != nil {
			return Result{}, err
		}
	}
	data := StatusData{Leases: []LeaseView{}}
	var lines []string
	for _, id := range ids {
		ls, err := l.readLease(id)
		if err != nil {
			return Result{}, err
		}
		v := l.view(ls)
		data.Leases = append(data.Leases, v)
		lines = append(lines, statusLine(v))
	}
	return Result{Data: data, Lines: lines, Next: []string{}}, nil
}

func statusLine(v LeaseView) string {
	if !v.Held {
		return v.Device + ": free"
	}
	line := fmt.Sprintf("%s: %s, held %s, holder %s", v.Device, v.Holder, v.HeldFor, v.HolderAlive)
	switch {
	case v.Stale:
		line += ", STALE (expired, holder gone)"
	case v.Expired:
		line += ", expired"
	}
	return line
}

// leaseIDs is every ledger device plus any lease file without a row.
func (l *Lab) leaseIDs() ([]string, error) {
	led, err := l.LoadLedger()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	ids := led.IDs()
	for _, id := range ids {
		seen[id] = true
	}
	entries, err := os.ReadDir(l.leasesDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if ok && !e.IsDir() && ValidID(id) && !seen[id] {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

// ReapData is `lease reap`'s payload.
type ReapData struct {
	Released []LeaseView `json:"released"`
	DryRun   bool        `json:"dryRun,omitempty"`
}

// Reap releases every lease that is expired AND whose holder is dead (or
// unknown). It never touches a live holder.
func (l *Lab) Reap(dryRun bool) (Result, error) {
	ids, err := l.leaseIDs()
	if err != nil {
		return Result{}, err
	}
	data := ReapData{Released: []LeaseView{}, DryRun: dryRun}
	var lines []string
	for _, id := range ids {
		ls, err := l.readLease(id)
		if err != nil || !l.stale(ls) {
			continue
		}
		v := l.view(ls)
		if !dryRun {
			if _, err := l.updateLease(id, "lease reap", func(cur *Lease) error {
				if cur.TokenSHA256 != ls.TokenSHA256 || !l.stale(cur) {
					return errNotStale
				}
				l.end(cur, "reaped: expired and its holder is gone")
				return nil
			}); err != nil {
				if errors.Is(err, errNotStale) {
					continue
				}
				return Result{}, err
			}
		}
		data.Released = append(data.Released, v)
		lines = append(lines, fmt.Sprintf("%s: reaped %s's lease", id, v.Holder))
	}
	if len(lines) == 0 {
		lines = []string{"no stale leases"}
	}
	return Result{Data: data, Lines: lines, Next: []string{}}, nil
}

var errNotStale = errors.New("lease is no longer stale")

// BreakOptions are `lease break`'s flags.
type BreakOptions struct{ Reason string }

// Break releases a lease without its token, allowed only when the holder is
// dead or the lease expired. A live holder answers DEVICE_LEASED: there is no
// force-steal.
func (l *Lab) Break(handle string, opts BreakOptions) (Result, error) {
	if strings.TrimSpace(opts.Reason) == "" {
		return Result{}, usage("lease break needs --reason", fmt.Sprintf("perflab lease break %s --reason \"<why>\" --json", handle))
	}
	id, _, err := l.resolve(handle)
	if err != nil {
		return Result{}, err
	}
	var broken LeaseView
	ls, err := l.updateLease(id, "lease break", func(ls *Lease) error {
		if !ls.Held() {
			return nil
		}
		if l.liveness(ls.Owner) != Dead && !ls.expired(l.now()) {
			return l.leasedErr(ls)
		}
		broken = l.view(ls)
		l.end(ls, "broken: "+opts.Reason)
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	line := id + " was not leased"
	if broken.Held {
		line = fmt.Sprintf("%s: broke %s's lease", id, broken.Holder)
	}
	return Result{Data: l.view(ls), Lines: []string{line}, Next: []string{fmt.Sprintf("perflab lease acquire %s --json", id)}}, nil
}

// HeldLease is one device under a live (non-stale) lease, as the
// claude-guards `machine:device-leased` rule reads it.
type HeldLease struct {
	DeviceID string   `json:"deviceId"`
	Platform Platform `json:"platform"`
	// Aliases are the device's ledger id plus every hardware handle.
	Aliases   []string  `json:"aliases"`
	Holder    *Owner    `json:"holder,omitempty"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// HeldLeases lists every device a lease holds, stale ones excluded. It takes
// no lock (lease writes are rename-atomic), so a PreToolUse hook can call it
// on every Bash command; a missing state dir is an empty answer.
func (l *Lab) HeldLeases() ([]HeldLease, error) {
	entries, err := os.ReadDir(l.leasesDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	led, err := l.LoadLedger()
	if err != nil {
		return nil, err
	}
	var out []HeldLease
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok || e.IsDir() || !ValidID(id) {
			continue
		}
		ls, err := l.readLease(id)
		if err != nil || !ls.Held() || l.stale(ls) {
			continue
		}
		h := HeldLease{DeviceID: id, Aliases: []string{id}, Holder: ls.Owner}
		if ls.ExpiresAt != nil {
			h.ExpiresAt = *ls.ExpiresAt
		}
		if d, ok := led.Devices[id]; ok {
			h.Platform = d.Platform
			h.Aliases = append(h.Aliases, d.Aliases()...)
		}
		out = append(out, h)
	}
	return out, nil
}

// LeaseFor answers which held lease, if any, covers a device handle (a
// ledger id, UDID, CoreDevice id or adb serial, case-insensitive).
func (l *Lab) LeaseFor(handle string) (HeldLease, bool, error) {
	held, err := l.HeldLeases()
	if err != nil {
		return HeldLease{}, false, err
	}
	for _, h := range held {
		for _, a := range h.Aliases {
			if strings.EqualFold(a, handle) {
				return h, true, nil
			}
		}
	}
	return HeldLease{}, false, nil
}

// statePresent reports whether the lab was ever used on this machine; the
// session hooks return at once when it was not.
func (l *Lab) statePresent() bool {
	_, err := os.Stat(filepath.Join(l.StateDir, "leases"))
	return err == nil
}

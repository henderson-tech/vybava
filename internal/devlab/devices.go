package devlab

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/henderson-tech/vybava/internal/runx"
)

// ScanRow is one live phone `device scan` found.
type ScanRow struct {
	Platform          Platform `json:"platform"`
	Name              string   `json:"name,omitempty"`
	UDID              string   `json:"udid,omitempty"`
	CoreDeviceID      string   `json:"coreDeviceId,omitempty"`
	Serial            string   `json:"serial,omitempty"`
	Model             string   `json:"model,omitempty"`
	ProductType       string   `json:"productType,omitempty"`
	OS                string   `json:"os,omitempty"`
	SDK               int      `json:"sdk,omitempty"`
	Transport         string   `json:"transport,omitempty"`
	Paired            bool     `json:"paired"`
	State             string   `json:"state,omitempty"`
	TunnelState       string   `json:"tunnelState,omitempty"`
	DeveloperMode     string   `json:"developerMode,omitempty"`
	Online            bool     `json:"online"`
	InstrumentsOnline *bool    `json:"instrumentsOnline,omitempty"`
	InLedger          string   `json:"inLedger,omitempty"`
	Suggested         string   `json:"suggestedId,omitempty"`
}

// ScanOptions are `device scan`'s flags.
type ScanOptions struct {
	Platform Platform // empty: both
}

// ScanData is `device scan`'s payload.
type ScanData struct {
	Devices []ScanRow `json:"devices"`
}

// Scan lists the phones attached or paired now, iOS from devicectl +
// xctrace, Android from adb, each marked with its ledger id when
// registered. A platform whose tool is missing or fails is a warning, never
// a failed scan.
func (l *Lab) Scan(ctx context.Context, opts ScanOptions) (Result, error) {
	if opts.Platform != "" && opts.Platform != PlatformIOS && opts.Platform != PlatformAndroid {
		return Result{}, usage(fmt.Sprintf("unknown platform %q", opts.Platform), "perflab device scan --platform ios|android --json")
	}
	led, err := l.LoadLedger()
	if err != nil {
		return Result{}, err
	}
	var rows []ScanRow
	var diags []runx.Diagnostic
	if opts.Platform != PlatformAndroid {
		r, d := l.scanIOS(ctx)
		rows, diags = append(rows, r...), append(diags, d...)
	}
	if opts.Platform != PlatformIOS {
		r, d := l.scanAndroid(ctx)
		rows, diags = append(rows, r...), append(diags, d...)
	}
	taken := map[string]bool{}
	for _, id := range led.IDs() {
		taken[id] = true
	}
	var next, lines []string
	for i := range rows {
		r := &rows[i]
		for _, h := range []string{r.UDID, r.CoreDeviceID, r.Serial} {
			if id, _, ok := led.Resolve(h); ok && h != "" {
				r.InLedger = id
				break
			}
		}
		if r.InLedger == "" {
			r.Suggested = suggestID(*r, taken)
			taken[r.Suggested] = true
			if r.Online || r.Paired {
				next = append(next, addCommand(*r))
			}
		}
		lines = append(lines, scanLine(*r))
	}
	diags = append(diags, ambiguousNames(rows)...)
	if rows == nil {
		rows = []ScanRow{}
	}
	return Result{Data: ScanData{Devices: rows}, Lines: lines, Diagnostics: diags, Next: next}, nil
}

func (l *Lab) scanIOS(ctx context.Context) ([]ScanRow, []runx.Diagnostic) {
	if _, err := l.LookPath("xcrun"); err != nil {
		return nil, []runx.Diagnostic{warnRow(DiagToolMissing, "xcrun is not on PATH: iOS devices were not scanned", "xcode-select --install")}
	}
	devices, err := l.devicectlDevices(ctx)
	if err != nil {
		return nil, []runx.Diagnostic{warnRow(DiagDeviceOffline, "iOS devices were not scanned: "+err.Error(), "xcrun devicectl list devices")}
	}
	var diags []runx.Diagnostic
	xt, xerr := l.xctraceDevices(ctx)
	if xerr != nil {
		diags = append(diags, warnRow(DiagDeviceOffline, "xctrace list devices failed, screen state unknown: "+xerr.Error(), ""))
	}
	var rows []ScanRow
	for _, d := range devices {
		if !d.phone() {
			continue
		}
		r := ScanRow{
			Platform:      PlatformIOS,
			Name:          d.DeviceProperties.Name,
			UDID:          d.HardwareProperties.UDID,
			CoreDeviceID:  d.Identifier,
			Model:         d.HardwareProperties.MarketingName,
			ProductType:   d.HardwareProperties.ProductType,
			OS:            d.DeviceProperties.OSVersionNumber,
			Transport:     d.ConnectionProperties.TransportType,
			Paired:        d.ConnectionProperties.PairingState == "paired",
			TunnelState:   d.ConnectionProperties.TunnelState,
			DeveloperMode: d.DeviceProperties.DeveloperModeStatus,
			Online:        d.reachable(),
		}
		if xerr == nil {
			_, online := xt.Online[r.UDID]
			r.InstrumentsOnline = &online
		}
		rows = append(rows, r)
	}
	return rows, diags
}

func (l *Lab) scanAndroid(ctx context.Context) ([]ScanRow, []runx.Diagnostic) {
	if _, err := l.LookPath("adb"); err != nil {
		return nil, []runx.Diagnostic{warnRow(DiagToolMissing, "adb is not on PATH: Android devices were not scanned", "brew install --cask android-platform-tools")}
	}
	devices, err := l.adbDevices(ctx)
	if err != nil {
		return nil, []runx.Diagnostic{warnRow(DiagDeviceOffline, "Android devices were not scanned: "+err.Error(), "adb devices -l")}
	}
	// A leased phone may be mid-measurement: perflab is exempt from the
	// device-leased guard because it honours leases, so a tokenless scan
	// reads only adb's host-side listing for it, never an in-device getprop.
	leased := map[string]bool{}
	if held, err := l.HeldLeases(); err == nil {
		for _, h := range held {
			for _, a := range h.Aliases {
				leased[strings.ToLower(a)] = true
			}
		}
	}
	var rows []ScanRow
	for _, d := range devices {
		r := ScanRow{Platform: PlatformAndroid, Serial: d.Serial, State: d.State, Model: strings.ReplaceAll(d.Model, "_", "-"), Transport: d.Transport, Paired: d.State != "unauthorized", Online: d.State == "device"}
		if r.Online && !leased[strings.ToLower(d.Serial)] {
			if id, err := l.androidIdentity(ctx, d.Serial); err == nil {
				r.Model, r.OS, r.SDK = orElse(id.Model, r.Model), id.OS, id.SDK
			}
		}
		rows = append(rows, r)
	}
	return rows, nil
}

func orElse(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// suggestID derives a short ledger id from the model ("iPhone 11" ->
// iphone11, "SM-G980F" -> sm-g980f), suffixed when taken.
func suggestID(r ScanRow, taken map[string]bool) string {
	var b strings.Builder
	for _, c := range strings.ToLower(orElse(r.Model, string(r.Platform))) {
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
			b.WriteRune(c)
		case c == '-' && b.Len() > 0:
			b.WriteRune(c)
		}
	}
	base := b.String()
	if base == "" || base[0] < 'a' || base[0] > 'z' {
		base = string(r.Platform) + base
	}
	if len(base) > 24 {
		base = base[:24]
	}
	id := base
	for n := 2; taken[id]; n++ {
		id = fmt.Sprintf("%s-%d", base, n)
	}
	return id
}

func addCommand(r ScanRow) string {
	if r.Platform == PlatformAndroid {
		return fmt.Sprintf("perflab device add %s --serial %s --json", r.Suggested, r.Serial)
	}
	return fmt.Sprintf("perflab device add %s --udid %s --json", r.Suggested, r.UDID)
}

func scanLine(r ScanRow) string {
	handle := r.UDID
	if r.Platform == PlatformAndroid {
		handle = r.Serial
	}
	state := "offline"
	if r.Online {
		state = "online"
	}
	ledger := "not in ledger"
	if r.InLedger != "" {
		ledger = "ledger " + r.InLedger
	}
	return fmt.Sprintf("%s %s %s (%s) %s, %s", r.Platform, orElse(r.Model, r.Name), r.OS, handle, state, ledger)
}

// ambiguousNames warns when two online phones share a name: names never
// resolve, the UDID does.
func ambiguousNames(rows []ScanRow) []runx.Diagnostic {
	byName := map[string][]string{}
	for _, r := range rows {
		if r.Name != "" && r.Online {
			byName[r.Name] = append(byName[r.Name], orElse(r.UDID, r.Serial))
		}
	}
	var names []string
	for n, ids := range byName {
		if len(ids) > 1 {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	var out []runx.Diagnostic
	for _, n := range names {
		out = append(out, warnRow(DiagDeviceAmbiguous, fmt.Sprintf("%d online devices are named %q: %s", len(byName[n]), n, strings.Join(byName[n], ", ")), "register and address them by UDID: perflab device add <id> --udid <udid>"))
	}
	return out
}

// AddOptions are `device add`'s flags. Pointers keep a re-run from clearing
// what it does not name.
type AddOptions struct {
	UDID            string
	CoreDeviceID    string
	Serial          string
	Label           string
	ExpectHz        int
	ProtectPackages []string
	Personal        *bool
	Notes           *string
}

// Add registers a phone, or updates its row. Identity comes from the live
// scan when the phone is attached; a phone the scan cannot see is still
// registered (warning), so an iPhone 8 can be added before it is plugged in.
func (l *Lab) Add(ctx context.Context, id string, opts AddOptions) (Result, error) {
	if !ValidID(id) {
		return Result{}, usage(fmt.Sprintf("device id %q must match [a-z][a-z0-9-]{0,31}", id), "perflab device add s20 --serial <serial> --json")
	}
	ios := opts.UDID != "" || opts.CoreDeviceID != ""
	if ios == (opts.Serial != "") {
		return Result{}, usage("name the device by --udid (iOS) or --serial (Android), exactly one", fmt.Sprintf("perflab device scan --json  (then perflab device add %s --udid <udid> | --serial <serial>)", id))
	}
	if opts.ExpectHz != 0 && opts.ExpectHz != 60 && opts.ExpectHz != 90 && opts.ExpectHz != 120 {
		return Result{}, usage(fmt.Sprintf("--expect-hz %d is not 60, 90 or 120", opts.ExpectHz), fmt.Sprintf("perflab device add %s --expect-hz 120 --json", id))
	}
	live := Device{AddedAt: l.now()}
	var diags []runx.Diagnostic
	if ios {
		live.Platform = PlatformIOS
		live.HardwareUDID, live.CoreDeviceID = opts.UDID, opts.CoreDeviceID
		if d, err := l.liveIOS(ctx, opts.UDID, opts.CoreDeviceID); err != nil {
			if (live.CoreDeviceID == "" || live.HardwareUDID == "") && !l.registered(id, &live) {
				return Result{}, diag(DiagDeviceOffline, fmt.Sprintf("devicectl does not list %s: %v", orElse(opts.UDID, opts.CoreDeviceID), err),
					"pair the phone with this Mac (plug it in, trust the Mac), then: perflab device scan --platform ios --json")
			}
			diags = append(diags, warnRow(DiagDeviceOffline, "registered without a live read: "+err.Error(), fmt.Sprintf("perflab device probe %s --json", id)))
		} else {
			live.HardwareUDID, live.CoreDeviceID = d.HardwareProperties.UDID, d.Identifier
			live.Label, live.Model, live.ProductType = d.HardwareProperties.MarketingName, d.HardwareProperties.MarketingName, d.HardwareProperties.ProductType
			live.OS, live.Transport = d.DeviceProperties.OSVersionNumber, d.ConnectionProperties.TransportType
		}
	} else {
		live.Platform = PlatformAndroid
		live.Serial = opts.Serial
		if aid, err := l.liveAndroid(ctx, opts.Serial); err != nil {
			diags = append(diags, warnRow(DiagDeviceOffline, "registered without a live read: "+err.Error(), fmt.Sprintf("perflab device probe %s --json", id)))
		} else {
			live.Model, live.OS, live.SDK, live.Resolution = aid.Model, aid.OS, aid.SDK, aid.Resolution
			live.Label = strings.TrimSpace(titleCase(aid.Manufacturer) + " " + aid.Model)
		}
	}
	var row Device
	err := l.updateLedger("device add", func(led *Ledger) error {
		for _, other := range led.IDs() {
			if other != id && sameDevice(led.Devices[other], &live) {
				return diag(DiagDeviceExists, fmt.Sprintf("%s is already registered as %s", strings.Join(live.Aliases(), ", "), other), fmt.Sprintf("perflab device show %s --json", other))
			}
		}
		cur, exists := led.Devices[id]
		if exists && !sameDevice(cur, &live) {
			return diag(DiagDeviceExists, fmt.Sprintf("%s already names another device (%s)", id, strings.Join(cur.Aliases(), ", ")), fmt.Sprintf("perflab device show %s --json", id))
		}
		if !exists {
			cur = &Device{Platform: live.Platform, AddedAt: live.AddedAt}
			led.Devices[id] = cur
		}
		mergeLive(cur, &live)
		if opts.Label != "" {
			cur.Label = opts.Label
		}
		if opts.ExpectHz != 0 {
			cur.ExpectHz = opts.ExpectHz
		}
		for _, p := range opts.ProtectPackages {
			if !cur.Protects(p) {
				cur.ProtectedPackages = append(cur.ProtectedPackages, p)
			}
		}
		if opts.Personal != nil {
			cur.Personal = *opts.Personal
		}
		if opts.Notes != nil {
			cur.Notes = *opts.Notes
		}
		row = *cur
		return nil
	})
	if err != nil {
		return Result{}, err
	}
	if row.ExpectHz == 0 {
		diags = append(diags, infoRow(DiagRefreshRateMismatch, id+" has no expectHz, so probe cannot check its refresh rate", fmt.Sprintf("perflab device add %s --expect-hz 60|120 --json", id)))
	}
	return Result{
		Data:        DeviceView{ID: id, Device: row},
		Lines:       []string{fmt.Sprintf("%s: %s %s %s", id, row.Platform, orElse(row.Label, row.Model), strings.Join(row.Aliases(), " "))},
		Diagnostics: diags,
		Next:        []string{fmt.Sprintf("perflab device probe %s --json", id)},
	}, nil
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// sameDevice: two rows are one phone when any hardware handle matches.
func sameDevice(a, b *Device) bool {
	if a.Platform != b.Platform {
		return false
	}
	for _, x := range a.Aliases() {
		for _, y := range b.Aliases() {
			if strings.EqualFold(x, y) {
				return true
			}
		}
	}
	return false
}

// mergeLive copies the live read's non-empty identity fields onto the row.
func mergeLive(cur, live *Device) {
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	set(&cur.HardwareUDID, live.HardwareUDID)
	set(&cur.CoreDeviceID, live.CoreDeviceID)
	set(&cur.Serial, live.Serial)
	set(&cur.Model, live.Model)
	set(&cur.ProductType, live.ProductType)
	set(&cur.OS, live.OS)
	set(&cur.Transport, live.Transport)
	set(&cur.Resolution, live.Resolution)
	if cur.Label == "" {
		cur.Label = live.Label
	}
	if live.SDK != 0 {
		cur.SDK = live.SDK
	}
}

// registered: the id already names this phone, so a re-run that only
// changes flags needs no live read.
func (l *Lab) registered(id string, live *Device) bool {
	led, err := l.LoadLedger()
	if err != nil {
		return false
	}
	cur, ok := led.Devices[id]
	return ok && sameDevice(cur, live)
}

func (l *Lab) liveIOS(ctx context.Context, udid, coreID string) (devicectlDevice, error) {
	if _, err := l.LookPath("xcrun"); err != nil {
		return devicectlDevice{}, fmt.Errorf("xcrun is not on PATH")
	}
	devices, err := l.devicectlDevices(ctx)
	if err != nil {
		return devicectlDevice{}, err
	}
	for _, d := range devices {
		if (udid != "" && strings.EqualFold(d.HardwareProperties.UDID, udid)) || (coreID != "" && strings.EqualFold(d.Identifier, coreID)) {
			return d, nil
		}
	}
	return devicectlDevice{}, fmt.Errorf("not among the %d devices devicectl lists", len(devices))
}

func (l *Lab) liveAndroid(ctx context.Context, serial string) (androidIdentity, error) {
	if _, err := l.LookPath("adb"); err != nil {
		return androidIdentity{}, fmt.Errorf("adb is not on PATH")
	}
	devices, err := l.adbDevices(ctx)
	if err != nil {
		return androidIdentity{}, err
	}
	for _, d := range devices {
		if d.Serial == serial {
			if d.State != "device" {
				return androidIdentity{}, fmt.Errorf("adb lists %s as %s", serial, d.State)
			}
			return l.androidIdentity(ctx, serial)
		}
	}
	return androidIdentity{}, fmt.Errorf("adb does not list %s", serial)
}

// DeviceView is one ledger row with its id, and with its lease when shown.
type DeviceView struct {
	ID string `json:"id"`
	Device
	Lease *LeaseView `json:"lease,omitempty"`
	// Online is the live read `device list` merges in (nil: not read).
	Online *bool `json:"online,omitempty"`
}

// ListData is `device list`'s payload.
type ListData struct {
	Devices []DeviceView `json:"devices"`
}

// List returns the ledger rows with their lease holders, merged with the
// live scan's online state when live is set.
func (l *Lab) List(ctx context.Context, live bool) (Result, error) {
	led, err := l.LoadLedger()
	if err != nil {
		return Result{}, err
	}
	var diags []runx.Diagnostic
	online := map[string]bool{}
	if live && len(led.Devices) > 0 {
		scan, err := l.Scan(ctx, ScanOptions{})
		if err != nil {
			return Result{}, err
		}
		diags = append(diags, scan.Diagnostics...)
		for _, r := range scan.Data.(ScanData).Devices {
			if r.InLedger != "" {
				online[r.InLedger] = r.Online
			}
		}
	}
	data := ListData{Devices: []DeviceView{}}
	var lines []string
	for _, id := range led.IDs() {
		ls, err := l.readLease(id)
		if err != nil {
			return Result{}, err
		}
		v := l.view(ls)
		dv := DeviceView{ID: id, Device: *led.Devices[id], Lease: &v}
		if live {
			o := online[id]
			dv.Online = &o
		}
		data.Devices = append(data.Devices, dv)
		lines = append(lines, fmt.Sprintf("%s  %s %s  %s", id, dv.Platform, orElse(dv.Label, dv.Model), statusLine(v)))
	}
	var next []string
	if len(data.Devices) == 0 {
		next = []string{"perflab device scan --json"}
	}
	return Result{Data: data, Lines: lines, Diagnostics: diags, Next: next}, nil
}

// Show returns one row, its lease, last probe and last installed variant.
func (l *Lab) Show(handle string) (Result, error) {
	id, dev, err := l.resolve(handle)
	if err != nil {
		return Result{}, err
	}
	ls, err := l.readLease(id)
	if err != nil {
		return Result{}, err
	}
	v := l.view(ls)
	next := []string{fmt.Sprintf("perflab lease acquire %s --json", id)}
	if v.Held {
		next = []string{fmt.Sprintf("perflab lease status %s --json", id)}
	}
	return Result{Data: DeviceView{ID: id, Device: *dev, Lease: &v}, Lines: []string{fmt.Sprintf("%s  %s %s  %s", id, dev.Platform, orElse(dev.Label, dev.Model), statusLine(v))}, Next: next}, nil
}

// RemoveOptions are `device remove`'s flags.
type RemoveOptions struct{ Yes bool }

// Remove deletes a row (and its lease record); refused while a live lease
// holds the device.
func (l *Lab) Remove(handle string, opts RemoveOptions) (Result, error) {
	id, _, err := l.resolve(handle)
	if err != nil {
		return Result{}, err
	}
	if !opts.Yes {
		return Result{}, usage("device remove deletes the ledger row; confirm it", fmt.Sprintf("perflab device remove %s --yes --json", id))
	}
	h, err := l.lockLease(id, "device remove")
	if err != nil {
		return Result{}, err
	}
	defer h.Release()
	ls, err := l.readLease(id)
	if err != nil {
		return Result{}, err
	}
	if ls.Held() && !l.stale(ls) {
		return Result{}, l.leasedErr(ls)
	}
	if err := l.updateLedger("device remove", func(led *Ledger) error {
		delete(led.Devices, id)
		return nil
	}); err != nil {
		return Result{}, err
	}
	if err := os.Remove(l.leasePath(id)); err != nil && !os.IsNotExist(err) {
		return Result{}, err
	}
	return Result{Data: map[string]string{"removed": id}, Lines: []string{id + " removed"}, Next: []string{}}, nil
}

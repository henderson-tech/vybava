package devlab

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// SchemaVersion is devices.json's version; bumped only on a breaking shape
// change.
const SchemaVersion = 1

// Ledger is devices.json: every physical phone this Mac has registered.
// It never stores a marketing-name lookup key.
type Ledger struct {
	SchemaVersion int                `json:"schemaVersion"`
	Devices       map[string]*Device `json:"devices"`
}

// Device is one ledger row. Its aliases (HardwareUDID, CoreDeviceID,
// Serial) are the only handles besides the id.
type Device struct {
	Platform    Platform `json:"platform"`
	Label       string   `json:"label,omitempty"`
	ProductType string   `json:"productType,omitempty"`
	Model       string   `json:"model,omitempty"`
	// HardwareUDID is what xctrace, Appium, the tunnel registry and result
	// JSON use; CoreDeviceID is devicectl's only handle.
	HardwareUDID      string           `json:"hardwareUdid,omitempty"`
	CoreDeviceID      string           `json:"coreDeviceId,omitempty"`
	Serial            string           `json:"serial,omitempty"`
	OS                string           `json:"os,omitempty"`
	SDK               int              `json:"sdk,omitempty"`
	ExpectHz          int              `json:"expectHz,omitempty"`
	Transport         string           `json:"transport,omitempty"`
	Resolution        string           `json:"resolution,omitempty"`
	SocClusters       map[string][]int `json:"socClusters,omitempty"`
	Personal          bool             `json:"personal,omitempty"`
	ProtectedPackages []string         `json:"protectedPackages,omitempty"`
	Notes             string           `json:"notes,omitempty"`
	AddedAt           time.Time        `json:"addedAt"`
	LastProbe         *ProbeSnapshot   `json:"lastProbe,omitempty"`
}

// ProbeSnapshot is the last probe's headline, kept on the row.
type ProbeSnapshot struct {
	At            time.Time `json:"at"`
	Online        bool      `json:"online"`
	RefreshHz     int       `json:"refreshHz,omitempty"`
	DeveloperMode string    `json:"developerMode,omitempty"`
	Tunnel        string    `json:"tunnel,omitempty"`
	Locked        *bool     `json:"locked,omitempty"`
}

// Aliases are the row's non-empty hardware handles.
func (d *Device) Aliases() []string {
	var out []string
	for _, a := range []string{d.HardwareUDID, d.CoreDeviceID, d.Serial} {
		if a != "" {
			out = append(out, a)
		}
	}
	return out
}

// Protects reports whether pkg is one of the row's protected packages.
func (d *Device) Protects(pkg string) bool {
	for _, p := range d.ProtectedPackages {
		if p == pkg {
			return true
		}
	}
	return false
}

// idPattern keeps ids short, lowercase and shell-safe (`s20`, `iphone11`).
var idPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// ValidID reports whether id can name a ledger row.
func ValidID(id string) bool { return idPattern.MatchString(id) }

// LoadLedger reads devices.json; a missing file is an empty ledger.
func (l *Lab) LoadLedger() (*Ledger, error) {
	b, err := os.ReadFile(l.ledgerPath())
	if errors.Is(err, os.ErrNotExist) {
		return &Ledger{SchemaVersion: SchemaVersion, Devices: map[string]*Device{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read device ledger: %w", err)
	}
	var led Ledger
	if err := json.Unmarshal(b, &led); err != nil {
		return nil, fmt.Errorf("parse %s: %w", l.ledgerPath(), err)
	}
	if led.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("%s has schemaVersion %d; this perflab reads %d", l.ledgerPath(), led.SchemaVersion, SchemaVersion)
	}
	if led.Devices == nil {
		led.Devices = map[string]*Device{}
	}
	return &led, nil
}

// saveLedger writes devices.json atomically; callers hold the ledger lock.
func (l *Lab) saveLedger(led *Ledger) error {
	led.SchemaVersion = SchemaVersion
	return writeJSONAtomic(l.ledgerPath(), led)
}

// updateLedger is the one read-modify-write path for devices.json.
func (l *Lab) updateLedger(verb string, fn func(*Ledger) error) error {
	h, err := l.lockLedger(verb)
	if err != nil {
		return err
	}
	defer h.Release()
	led, err := l.LoadLedger()
	if err != nil {
		return err
	}
	if err := fn(led); err != nil {
		return err
	}
	return l.saveLedger(led)
}

// Resolve maps a handle (ledger id or any registered alias, matched
// case-insensitively) to its row. Marketing names never resolve.
func (led *Ledger) Resolve(handle string) (string, *Device, bool) {
	h := strings.TrimSpace(handle)
	if h == "" {
		return "", nil, false
	}
	if d, ok := led.Devices[h]; ok {
		return h, d, true
	}
	for _, id := range led.IDs() {
		d := led.Devices[id]
		for _, a := range d.Aliases() {
			if strings.EqualFold(a, h) {
				return id, d, true
			}
		}
	}
	return "", nil, false
}

// IDs are the row ids in sorted order.
func (led *Ledger) IDs() []string {
	ids := make([]string, 0, len(led.Devices))
	for id := range led.Devices {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// resolve loads the ledger and resolves a handle, DEVICE_UNKNOWN otherwise.
func (l *Lab) resolve(handle string) (string, *Device, error) {
	if strings.TrimSpace(handle) == "" {
		return "", nil, usage("a device handle is required", "perflab device list --json")
	}
	led, err := l.LoadLedger()
	if err != nil {
		return "", nil, err
	}
	id, d, ok := led.Resolve(handle)
	if !ok {
		return "", nil, unknownDevice(handle)
	}
	return id, d, nil
}

// writeJSONAtomic writes temp file -> fsync -> rename, so a reader never
// sees a torn document.
func writeJSONAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

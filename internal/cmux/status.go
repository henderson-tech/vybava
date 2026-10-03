package cmux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"howett.net/plist"
)

// State is the one-word answer to "can fleet act through cmux right now".
type State string

const (
	// StateOK: reachable, recent enough, open to processes outside cmux.
	StateOK State = "ok"
	// StateUnreachable: no socket, or nothing listening — cmux is not running.
	StateUnreachable State = "unreachable"
	// StateDenied: cmux closed the connection unanswered — its access mode
	// admits only processes cmux started.
	StateDenied State = "denied"
	// StateRestricted: this caller got in, but the access mode would refuse
	// Fleet.app and the watch LaunchAgent.
	StateRestricted State = "restricted"
	// StateOutdated: older than MinVersion, or a required method is missing.
	StateOutdated State = "outdated"
)

// Status is the reachability report fleet publishes and prints.
type Status struct {
	State      State    `json:"state"`
	Socket     string   `json:"socket"`
	Version    string   `json:"version,omitempty"`
	AccessMode string   `json:"accessMode,omitempty"`
	Missing    []string `json:"missing,omitempty"`
	Detail     string   `json:"detail,omitempty"`
	Fix        string   `json:"fix,omitempty"`
}

// OK reports whether actions may go through.
func (s Status) OK() bool { return s.State == StateOK }

// openAccessMode is the access mode that admits a process cmux did not
// start (Settings → Socket Control → Automation).
const openAccessMode = "automation"

// Check reports whether cmux is reachable, recent enough and open to this
// kind of client. It never fails: every problem is a Status.
func (c Client) Check(ctx context.Context) Status {
	status := Status{Socket: c.Socket}
	caps, err := c.Capabilities(ctx)
	if err != nil {
		var unreachable *UnreachableError
		switch {
		case errors.As(err, &unreachable) && unreachable.Closed:
			status.State, status.Detail = StateDenied, err.Error()
			status.Fix = "cmux Settings → Socket Control → Automation (admits clients cmux did not start)"
		case errors.Is(err, os.ErrDeadlineExceeded):
			status.State, status.Detail = StateUnreachable, "cmux did not answer in time: "+err.Error()
			status.Fix = "cmux is busy; the next read retries"
		case errors.As(err, &unreachable):
			status.State, status.Detail = StateUnreachable, err.Error()
			status.Fix = "start cmux"
		default:
			status.State, status.Detail = StateOutdated, err.Error()
			status.Fix = "update cmux to " + MinVersion + " or newer"
		}
		return status
	}
	status.AccessMode = caps.AccessMode
	if id, err := c.Identify(ctx); err == nil && id.BundlePath != "" {
		status.Version, _ = BundleVersion(id.BundlePath)
	}
	have := map[string]bool{}
	for _, m := range caps.Methods {
		have[m] = true
	}
	for _, m := range RequiredMethods {
		if !have[m] {
			status.Missing = append(status.Missing, m)
		}
	}
	switch {
	case len(status.Missing) > 0 || (status.Version != "" && Older(status.Version, MinVersion)):
		status.State = StateOutdated
		status.Detail = fmt.Sprintf("cmux %s lacks %s", orUnknown(status.Version), strings.Join(append([]string{"version " + MinVersion}, status.Missing...), ", "))
		status.Fix = "update cmux to " + MinVersion + " or newer and restart it"
	case caps.AccessMode != openAccessMode:
		status.State = StateRestricted
		status.Detail = fmt.Sprintf("socket access mode is %q; Fleet.app and the watch agent are not started by cmux and will be refused", caps.AccessMode)
		status.Fix = "cmux Settings → Socket Control → Automation"
	default:
		status.State = StateOK
	}
	return status
}

func orUnknown(version string) string {
	if version == "" {
		return "(version unknown)"
	}
	return version
}

// BundleVersion reads CFBundleShortVersionString from an app bundle's
// Info.plist — no process spawned.
func BundleVersion(bundlePath string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(bundlePath, "Contents", "Info.plist"))
	if err != nil {
		return "", err
	}
	var info struct {
		Version string `plist:"CFBundleShortVersionString"`
	}
	if _, err := plist.Unmarshal(raw, &info); err != nil {
		return "", err
	}
	return info.Version, nil
}

// Older reports whether dotted version a sorts before b, numerically per
// component ("0.64.9" < "0.64.25"); a non-numeric component counts as 0.
func Older(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		x, y := component(pa, i), component(pb, i)
		if x != y {
			return x < y
		}
	}
	return false
}

func component(parts []string, i int) int {
	if i >= len(parts) {
		return 0
	}
	n, _ := strconv.Atoi(parts[i])
	return n
}

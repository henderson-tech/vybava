package devlab

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"
)

// devicectlList is the slice of `xcrun devicectl list devices
// --json-output` the lab reads (a file is devicectl's only machine output).
type devicectlList struct {
	Result struct {
		Devices []devicectlDevice `json:"devices"`
	} `json:"result"`
}

type devicectlDevice struct {
	Identifier           string `json:"identifier"`
	ConnectionProperties struct {
		PairingState  string `json:"pairingState"`
		TunnelState   string `json:"tunnelState"`
		TransportType string `json:"transportType"`
	} `json:"connectionProperties"`
	DeviceProperties struct {
		Name                string `json:"name"`
		OSVersionNumber     string `json:"osVersionNumber"`
		DeveloperModeStatus string `json:"developerModeStatus"`
		BootState           string `json:"bootState"`
	} `json:"deviceProperties"`
	HardwareProperties struct {
		UDID          string `json:"udid"`
		MarketingName string `json:"marketingName"`
		ProductType   string `json:"productType"`
		Platform      string `json:"platform"`
		Reality       string `json:"reality"`
	} `json:"hardwareProperties"`
}

// phone reports whether the row is an iOS device (watches are listed too).
func (d devicectlDevice) phone() bool {
	return d.HardwareProperties.Platform == "iOS"
}

// reachable: devicectl can talk to it now ("unavailable" is not attached,
// not on the same network, or not unlocked since it went away).
func (d devicectlDevice) reachable() bool {
	return d.ConnectionProperties.TunnelState != "" && d.ConnectionProperties.TunnelState != "unavailable"
}

func parseDevicectlList(b []byte) ([]devicectlDevice, error) {
	var list devicectlList
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("devicectl list devices: %w", err)
	}
	return list.Result.Devices, nil
}

// devicectlJSON runs a devicectl command whose result is the JSON file it
// writes, and returns that file's bytes.
func (l *Lab) devicectlJSON(ctx context.Context, timeout time.Duration, args ...string) ([]byte, CmdOut, error) {
	tmp, err := os.CreateTemp(l.TempDir(), "perflab-devicectl-*.json")
	if err != nil {
		return nil, CmdOut{}, err
	}
	path := tmp.Name()
	_ = tmp.Close()
	defer os.Remove(path)
	full := append(append([]string{"xcrun", "devicectl"}, args...), "--json-output", path)
	out, err := l.run(ctx, timeout, full...)
	if err != nil {
		return nil, out, err
	}
	if out.Code != 0 {
		return nil, out, fmt.Errorf("xcrun devicectl %s: %s", strings.Join(args, " "), stderrTail(out))
	}
	b, err := os.ReadFile(path)
	return b, out, err
}

func (l *Lab) devicectlDevices(ctx context.Context) ([]devicectlDevice, error) {
	b, _, err := l.devicectlJSON(ctx, 30*time.Second, "list", "devices")
	if err != nil {
		return nil, err
	}
	return parseDevicectlList(b)
}

// xctraceDevices is `xcrun xctrace list devices`: a phone with a sleeping
// screen moves from "Devices" to "Devices Offline". Keys are hardware UDIDs.
type xctraceDevices struct {
	Online  map[string]string
	Offline map[string]string
}

func parseXctraceDevices(out string) xctraceDevices {
	res := xctraceDevices{Online: map[string]string{}, Offline: map[string]string{}}
	var section map[string]string
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimSpace(raw)
		switch {
		case line == "":
			continue
		case strings.HasPrefix(line, "=="):
			switch strings.Trim(line, "= ") {
			case "Devices":
				section = res.Online
			case "Devices Offline":
				section = res.Offline
			default:
				section = nil // Simulators and anything newer
			}
			continue
		}
		if section == nil || !strings.HasSuffix(line, ")") {
			continue
		}
		open := strings.LastIndex(line, "(")
		if open < 0 {
			continue
		}
		udid := line[open+1 : len(line)-1]
		section[udid] = strings.TrimSpace(line[:open])
	}
	return res
}

func (l *Lab) xctraceDevices(ctx context.Context) (xctraceDevices, error) {
	out, err := l.run(ctx, 60*time.Second, "xcrun", "xctrace", "list", "devices")
	if err != nil {
		return xctraceDevices{}, err
	}
	if out.Code != 0 {
		return xctraceDevices{}, fmt.Errorf("xcrun xctrace list devices: %s", stderrTail(out))
	}
	return parseXctraceDevices(out.Stdout), nil
}

// DefaultTunnelRegistry is the RemoteXPC tunnel registry the root
// `appium driver run xcuitest tunnel-creation` serves; PERFLAB_TUNNEL_REGISTRY
// overrides it.
const DefaultTunnelRegistry = "http://127.0.0.1:42314/remotexpc/tunnels"

// TunnelCreationFix is the human's line for a missing tunnel: sudo drops
// PATH (appium and node not found) and HOME decides where the registry port
// is stored.
const TunnelCreationFix = "in a human terminal: sudo HOME=$HOME env \"PATH=$PATH\" appium driver run xcuitest tunnel-creation"

type tunnelRegistry struct {
	Status  string                 `json:"status"`
	Tunnels map[string]tunnelEntry `json:"tunnels"`
}

type tunnelEntry struct {
	UDID           string `json:"udid"`
	Address        string `json:"address"`
	RSDPort        int    `json:"rsdPort"`
	ConnectionType string `json:"connectionType"`
}

func parseTunnelRegistry(b []byte) (tunnelRegistry, error) {
	var reg tunnelRegistry
	if err := json.Unmarshal(b, &reg); err != nil {
		return reg, fmt.Errorf("tunnel registry: %w", err)
	}
	if reg.Status != "OK" {
		return reg, fmt.Errorf("tunnel registry status %q", reg.Status)
	}
	return reg, nil
}

func (l *Lab) tunnelRegistryURL() string {
	if u := l.Getenv("PERFLAB_TUNNEL_REGISTRY"); u != "" {
		return u
	}
	return DefaultTunnelRegistry
}

func (l *Lab) tunnels(ctx context.Context) (tunnelRegistry, error) {
	b, err := l.HTTPGet(ctx, l.tunnelRegistryURL())
	if err != nil {
		return tunnelRegistry{}, err
	}
	return parseTunnelRegistry(b)
}

// tunnelFor returns the registry's usable tunnel for a hardware UDID.
func (reg tunnelRegistry) tunnelFor(udid string) (tunnelEntry, bool) {
	for key, t := range reg.Tunnels {
		if strings.EqualFold(key, udid) && t.Address != "" && t.RSDPort > 0 {
			return t, true
		}
	}
	return tunnelEntry{}, false
}

// osMajor is the leading integer of a version string ("18.7.8" -> 18).
func osMajor(v string) int {
	n := 0
	for _, r := range v {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	return n
}

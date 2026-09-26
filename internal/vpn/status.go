package vpn

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/henderson-tech/vybava/internal/runx"
)

// Machine is the read-only view of the Mac that status needs; tests fake it.
type Machine interface {
	// AppState is WireGuard.app's view of the profile (`scutil --nc status`):
	// "Connected", "Disconnected", … — "" when the app has no such profile —
	// and the utun a connected profile runs on.
	AppState(ctx context.Context, name string) (state, iface string)
	// Service is launchd's system-domain job holding label.
	Service(ctx context.Context, label string) (Service, error)
	// Stat is a file's mtime, false when it does not exist; it never reads it.
	Stat(path string) (time.Time, bool)
	ReadFile(path string) (string, error)
	// RouteInterface is the interface the kernel routes host through.
	RouteInterface(ctx context.Context, host string) string
	// AskDNS returns nil when the server answers a query at all.
	AskDNS(ctx context.Context, addr string) error
	Dial(ctx context.Context, addr string) error
}

// Service is the parsed `launchctl print system/<label>`.
type Service struct {
	Loaded  bool
	Type    string // LaunchDaemon | Submitted | …
	Path    string // the plist, or "(submitted by launchctl[pid])"
	Running bool
	PID     int
}

// Daemon says which launchd job holds the tunnel. transient is a
// `launchctl submit` job: it works until the next reboot.
type Daemon struct {
	Kind      string `json:"kind"` // persistent | transient | other | none
	Installed bool   `json:"installed"`
	Running   bool   `json:"running"`
	PID       int    `json:"pid,omitempty"`
}

type Check struct {
	Target string `json:"target"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
}

// Status is one tunnel as the kernel sees it, beside what WireGuard.app and
// launchd claim — the three disagree, and only the route is the truth.
type Status struct {
	Name  string `json:"name"`
	State string `json:"state"` // up | degraded | down
	Via   string `json:"via"`   // wg-quick | app | none
	// Interface is the utun — wg-quick's or WireGuard.app's — the tunnel's DNS
	// (or first probe) routes through.
	Interface string  `json:"interface,omitempty"`
	Summary   string  `json:"summary"`
	App       string  `json:"app"` // WireGuard.app's state; absent without a profile
	Daemon    Daemon  `json:"daemon"`
	DNS       *Check  `json:"dns,omitempty"`
	Probes    []Check `json:"probes"`
	Log       string  `json:"log"`
}

// Inspect observes one tunnel. The DNS question and TCP probes run in
// parallel, each bounded by the Machine's own timeout.
func Inspect(ctx context.Context, m Machine, name string, p Profile) (Status, error) {
	app, appIface := m.AppState(ctx, name)
	s := Status{Name: name, App: app, Probes: make([]Check, len(p.Probes)), Log: LogPath(name)}
	if s.App == "" {
		s.App = "absent"
	}
	svc, err := m.Service(ctx, Label(name))
	if err != nil {
		return s, err
	}
	_, installed := m.Stat(PlistPath(name))
	s.Daemon = Daemon{Kind: "none", Installed: installed, Running: svc.Running, PID: svc.PID}
	switch {
	case !svc.Loaded:
	case svc.Type == "Submitted":
		s.Daemon.Kind = "transient"
	case svc.Path == PlistPath(name):
		s.Daemon.Kind = "persistent"
	default:
		s.Daemon.Kind = "other"
	}

	host := dnsHost(p.DNS)
	if host == "" && len(p.Probes) > 0 {
		host, _, _ = net.SplitHostPort(p.Probes[0])
	}
	route := m.RouteInterface(ctx, host)
	marker, wg := wgQuickOwns(m, name, route)

	var wait sync.WaitGroup
	check := func(c *Check, probe func(context.Context, string) error) {
		defer wait.Done()
		if err := probe(ctx, c.Target); err != nil {
			c.Error = err.Error()
		} else {
			c.OK = true
		}
	}
	if p.DNS != "" {
		s.DNS = &Check{Target: dnsAddr(p.DNS)}
		wait.Add(1)
		go check(s.DNS, m.AskDNS)
	}
	for i, target := range p.Probes {
		s.Probes[i] = Check{Target: target}
		wait.Add(1)
		go check(&s.Probes[i], m.Dial)
	}
	wait.Wait()

	switch {
	case wg:
		s.Via, s.Interface = "wg-quick", route
	case s.App == "Connected" && appIface != "" && appIface == route:
		s.Via, s.Interface = "app", route
	default:
		s.Via = "none"
	}
	var failed []string
	if s.DNS != nil && !s.DNS.OK {
		failed = append(failed, "DNS "+s.DNS.Target+" silent")
	}
	for _, c := range s.Probes {
		if !c.OK {
			failed = append(failed, c.Target+" unreachable")
		}
	}
	carrier := map[string]string{"wg-quick": "wg-quick (" + s.Interface + ")", "app": "WireGuard.app"}[s.Via]
	switch {
	case s.Via == "none" && marker:
		s.State, s.Summary = "down", "down: a wg-quick "+name+" marker exists, but its interface does not carry "+host+" (routed via "+orUnknown(route)+")"
	case s.Via == "none" && s.App == "Connected":
		s.State, s.Summary = "down", "down: WireGuard.app says Connected (on "+orUnknown(appIface)+"), but "+host+" routes via "+orUnknown(route)
	case s.Via == "none":
		s.State, s.Summary = "down", "down"
	case len(failed) > 0:
		s.State, s.Summary = "degraded", "degraded via "+carrier+": "+strings.Join(failed, ", ")
	default:
		s.State, s.Summary = "up", "up via "+carrier
	}
	return s, nil
}

// wgQuickOwns applies wg-quick's own get_real_interface rule to the routed
// interface: <name>.name and <iface>.sock exist and were written within 2 s
// of each other — one wireguard-go start writes both, so a marker older or
// newer than the socket is another run's. wireguard-go writes the marker
// root-only; the vybava supervisor makes it world-readable, and a readable
// marker must also name iface.
func wgQuickOwns(m Machine, name, iface string) (marker, owns bool) {
	path := filepath.Join(runDir, name+".name")
	written, marker := m.Stat(path)
	if !marker || iface == "" {
		return marker, false
	}
	sock, ok := m.Stat(filepath.Join(runDir, iface+".sock"))
	if d := sock.Unix() - written.Unix(); !ok || d >= 2 || d <= -2 {
		return true, false
	}
	if recorded, err := m.ReadFile(path); err == nil && strings.TrimSpace(recorded) != iface {
		return true, false
	}
	return true, true
}

func orUnknown(s string) string {
	if s == "" {
		return "no known interface"
	}
	return s
}

// Diagnose turns a status into the envelope's findings, each with the exact
// next command. Errors (exit 2): down, or a silent tunnel DNS.
func Diagnose(s Status) []runx.Diagnostic {
	var d []runx.Diagnostic
	add := func(code, severity, detail, fix string) {
		d = append(d, runx.Diagnostic{Code: code, Severity: severity, Detail: s.Name + ": " + detail, Fix: fix})
	}
	target := "system/" + Label(s.Name)
	switch {
	case s.Via != "none":
	case s.App == "Connected":
		add("VPN_DOWN", "error", "WireGuard.app says Connected, but its interface does not carry the tunnel's route", "turn "+s.Name+" off in WireGuard.app, then: vybava vpn install "+s.Name)
	case s.Daemon.Kind == "persistent":
		add("VPN_DOWN", "error", "the daemon runs but the tunnel is not up", "sudo tail -n 40 "+s.Log)
	case s.Daemon.Installed && s.Daemon.Kind == "none":
		add("VPN_DOWN", "error", "the LaunchDaemon is installed but not loaded", "sudo launchctl bootstrap system "+PlistPath(s.Name))
	default:
		add("VPN_DOWN", "error", "no tunnel carries it", "vybava vpn install "+s.Name)
	}
	if s.Via == "none" {
		return d
	}
	if s.DNS != nil && !s.DNS.OK {
		fix := "vybava vpn install " + s.Name
		if s.Daemon.Kind == "persistent" {
			fix = "sudo launchctl kickstart -k " + target
		}
		add("VPN_DNS_SILENT", "error", "the tunnel DNS "+s.DNS.Target+" did not answer ("+s.DNS.Error+"); every lookup /etc/resolver sends there stalls", fix)
	}
	for _, c := range s.Probes {
		if !c.OK {
			add("VPN_PROBE_FAILED", "warning", c.Target+" is unreachable through the tunnel ("+c.Error+")", "")
		}
	}
	if s.Via == "wg-quick" {
		switch s.Daemon.Kind {
		case "persistent":
		case "transient":
			add("VPN_NOT_PERSISTENT", "warning", "carried by a `launchctl submit` job that dies at the next reboot", "vybava vpn install "+s.Name)
		default:
			add("VPN_NOT_PERSISTENT", "warning", "carried by a wg-quick interface no vybava daemon owns", "vybava vpn install "+s.Name)
		}
		switch s.App {
		case "Connected":
			add("VPN_DUPLICATE", "warning", "WireGuard.app also runs this profile; one identity must not run twice", "turn "+s.Name+" off in WireGuard.app")
		case "absent":
		default:
			add("VPN_APP_UNAWARE", "info", "WireGuard.app and `scutil --nc` say "+s.App+" — expected: wg-quick carries this tunnel outside the app", "")
		}
	}
	return d
}

// System is the real Mac.
type System struct{}

func (System) AppState(ctx context.Context, name string) (string, string) {
	out, err := exec.CommandContext(ctx, "/usr/sbin/scutil", "--nc", "status", name).CombinedOutput()
	return appState(string(out), err)
}

// appState classifies one `scutil --nc status` run: "No service" is the only
// answer meaning the app has no such profile; any other failure is unknown,
// even one that printed nothing.
func appState(out string, err error) (string, string) {
	if err != nil && !strings.HasPrefix(strings.TrimSpace(out), "No service") {
		return "unknown (" + err.Error() + ")", ""
	}
	return ParseAppStatus(out)
}

// ParseAppStatus reads `scutil --nc status`: the first line is the state
// ("" for "No service"); a connected profile's extended status names its
// utun as InterfaceName.
func ParseAppStatus(out string) (state, iface string) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if strings.HasPrefix(lines[0], "No service") {
		return "", ""
	}
	for _, line := range lines[1:] {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "InterfaceName : "); ok && iface == "" {
			iface = value
		}
	}
	return lines[0], iface
}

func (System) Service(ctx context.Context, label string) (Service, error) {
	out, err := exec.CommandContext(ctx, "/bin/launchctl", "print", "system/"+label).CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "Could not find service") {
			return Service{}, nil
		}
		return Service{}, fmt.Errorf("launchctl print system/%s: %s", label, strings.TrimSpace(string(out)))
	}
	return ParseService(string(out)), nil
}

// ParseService reads the job's own top-level keys (one tab deep) from
// `launchctl print`; nested sections repeat the same key names.
func ParseService(out string) Service {
	s := Service{Loaded: true}
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "\t\t") {
			continue
		}
		key, value, ok := strings.Cut(strings.TrimSpace(line), " = ")
		if !ok {
			continue
		}
		switch key {
		case "type":
			s.Type = value
		case "path":
			s.Path = value
		case "state":
			s.Running = value == "running"
		case "pid":
			s.PID, _ = strconv.Atoi(value)
		}
	}
	return s
}

func (System) Stat(path string) (time.Time, bool) {
	info, err := os.Lstat(path)
	if err != nil {
		return time.Time{}, false
	}
	return info.ModTime(), true
}

func (System) ReadFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	return string(b), err
}

func (System) RouteInterface(ctx context.Context, host string) string {
	if host == "" {
		return ""
	}
	out, err := exec.CommandContext(ctx, "/sbin/route", "-n", "get", host).Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "interface: "); ok {
			return value
		}
	}
	return ""
}

const probeTimeout = 2 * time.Second

// AskDNS sends one root NS query over UDP; any reply with our id — even a
// refusal — proves the server answers.
func (System) AskDNS(ctx context.Context, addr string) error {
	conn, err := (&net.Dialer{Timeout: probeTimeout}).DialContext(ctx, "udp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(probeTimeout)); err != nil {
		return err
	}
	query := []byte{0, 0, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 2, 0, 1}
	if _, err := rand.Read(query[:2]); err != nil {
		return err
	}
	if _, err := conn.Write(query); err != nil {
		return err
	}
	reply := make([]byte, 512)
	for {
		n, err := conn.Read(reply)
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			return fmt.Errorf("no answer within %s", probeTimeout)
		}
		if err != nil {
			return err
		}
		if n >= 12 && reply[0] == query[0] && reply[1] == query[1] && reply[2]&0x80 != 0 {
			return nil
		}
	}
}

func (System) Dial(ctx context.Context, addr string) error {
	conn, err := (&net.Dialer{Timeout: probeTimeout}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

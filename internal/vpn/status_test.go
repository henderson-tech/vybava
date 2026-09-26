package vpn

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"
)

// file is one path on the fake Mac: its mtime in seconds, and its content
// when readable ("" = root-only, as wireguard-go writes a marker).
type file struct {
	at   int64
	body string
}

type fakeMac struct {
	app    string
	appIf  string // the utun scutil names for a connected profile
	svc    Service
	files  map[string]file
	route  string
	dnsErr error
}

func (f fakeMac) AppState(context.Context, string) (string, string) { return f.app, f.appIf }
func (f fakeMac) Service(context.Context, string) (Service, error)  { return f.svc, nil }
func (f fakeMac) Stat(path string) (time.Time, bool) {
	file, ok := f.files[path]
	return time.Unix(file.at, 0), ok
}
func (f fakeMac) ReadFile(path string) (string, error) {
	if file, ok := f.files[path]; ok && file.body != "" {
		return file.body, nil
	}
	return "", os.ErrPermission
}
func (f fakeMac) RouteInterface(context.Context, string) string { return f.route }
func (f fakeMac) AskDNS(context.Context, string) error          { return f.dnsErr }
func (f fakeMac) Dial(_ context.Context, addr string) error     { return nil }

func TestInspectClassifiesByRouteNotByAppState(t *testing.T) {
	const marker, sock11, sock12 = "/var/run/wireguard/lovinka-admin.name", "/var/run/wireguard/utun11.sock", "/var/run/wireguard/utun12.sock"
	wgUp := map[string]file{marker: {at: 1000}, sock11: {at: 1000}}
	installed := map[string]file{marker: {at: 1000, body: "utun11\n"}, sock11: {at: 1001}, PlistPath("lovinka-admin"): {}}
	cases := []struct {
		name, state, summary string
		mac                  fakeMac
		codes                []string
	}{
		{
			name:  "the 2026-09-22 recovery: app Disconnected, transient job on utun11",
			mac:   fakeMac{app: "Disconnected", svc: Service{Loaded: true, Type: "Submitted", Running: true, PID: 48756}, files: wgUp, route: "utun11"},
			state: "up", summary: "up via wg-quick (utun11)",
			codes: []string{"VPN_NOT_PERSISTENT", "VPN_APP_UNAWARE"},
		},
		{
			name:  "persistent daemon, healthy",
			mac:   fakeMac{app: "Disconnected", svc: Service{Loaded: true, Type: "LaunchDaemon", Path: PlistPath("lovinka-admin"), Running: true}, files: installed, route: "utun11"},
			state: "up", summary: "up via wg-quick (utun11)",
			codes: []string{"VPN_APP_UNAWARE"},
		},
		{
			name:  "persistent daemon, tunnel DNS silent",
			mac:   fakeMac{svc: Service{Loaded: true, Type: "LaunchDaemon", Path: PlistPath("lovinka-admin"), Running: true}, files: installed, route: "utun11", dnsErr: errors.New("no answer within 2s")},
			state: "degraded", summary: "degraded via wg-quick (utun11): DNS 10.8.1.1:53 silent",
			codes: []string{"VPN_DNS_SILENT"},
		},
		{
			name:  "after a reboot: nothing up, 10.8.1.1 leaves via en0",
			mac:   fakeMac{app: "Disconnected", route: "en0", dnsErr: errors.New("no answer within 2s")},
			state: "down", summary: "down",
			codes: []string{"VPN_DOWN"},
		},
		{
			name:  "crossed tunnels: a stale marker, the route through another tunnel's utun",
			mac:   fakeMac{app: "Disconnected", svc: Service{Loaded: true, Type: "Submitted", Running: true}, files: map[string]file{marker: {at: 1000}, sock11: {at: 1000}, sock12: {at: 5000}}, route: "utun12"},
			state: "down", summary: "down: a wg-quick lovinka-admin marker exists, but its interface does not carry 10.8.1.1 (routed via utun12)",
			codes: []string{"VPN_DOWN"},
		},
		{
			name:  "crossed tunnels started together: the readable marker names another utun",
			mac:   fakeMac{svc: Service{Loaded: true, Type: "LaunchDaemon", Path: PlistPath("lovinka-admin"), Running: true}, files: map[string]file{marker: {at: 1000, body: "utun11\n"}, sock11: {at: 1000}, sock12: {at: 1001}, PlistPath("lovinka-admin"): {}}, route: "utun12"},
			state: "down", summary: "down: a wg-quick lovinka-admin marker exists, but its interface does not carry 10.8.1.1 (routed via utun12)",
			codes: []string{"VPN_DOWN"},
		},
		{
			name:  "WireGuard.app carries it",
			mac:   fakeMac{app: "Connected", appIf: "utun4", route: "utun4"},
			state: "up", summary: "up via WireGuard.app",
		},
		{
			name:  "WireGuard.app says Connected, but 10.8.1.1 leaves via en0",
			mac:   fakeMac{app: "Connected", appIf: "utun4", route: "en0"},
			state: "down", summary: "down: WireGuard.app says Connected (on utun4), but 10.8.1.1 routes via en0",
			codes: []string{"VPN_DOWN"},
		},
	}
	p := Profile{Ref: "onyx://WireGuard/x/Configuration", Probes: []string{"10.8.1.1:443"}, DNS: "10.8.1.1"}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, err := Inspect(context.Background(), c.mac, "lovinka-admin", p)
			if err != nil {
				t.Fatal(err)
			}
			if s.State != c.state || s.Summary != c.summary {
				t.Fatalf("got %s / %q, want %s / %q", s.State, s.Summary, c.state, c.summary)
			}
			var codes []string
			for _, d := range Diagnose(s) {
				codes = append(codes, d.Code)
			}
			if !reflect.DeepEqual(codes, c.codes) {
				t.Fatalf("diagnostics %v, want %v", codes, c.codes)
			}
		})
	}
}

func TestParseAppStatusNamesTheConnectedUtun(t *testing.T) {
	connected := "Connected\nExtended Status <dictionary> {\n  DNSServers : <array> {\n    0 : 10.70.111.1\n  }\n  IPv4 : <dictionary> {\n    Addresses : <array> {\n      0 : 10.70.111.23\n    }\n    InterfaceName : utun7\n  }\n}\n"
	disconnected := "Disconnected\nExtended Status <dictionary> {\n  IsPrimaryInterface : 0\n  Status : 0\n}\n"
	for out, want := range map[string][2]string{
		connected:      {"Connected", "utun7"},
		disconnected:   {"Disconnected", ""},
		"No service\n": {"", ""},
	} {
		if state, iface := ParseAppStatus(out); state != want[0] || iface != want[1] {
			t.Errorf("got %q/%q, want %q/%q from:\n%s", state, iface, want[0], want[1], out)
		}
	}
}

// Only "No service" means the app has no profile; any other failed scutil
// run is unknown, even one that printed nothing.
func TestAppStateIsUnknownWhenScutilFailsSilently(t *testing.T) {
	failed := errors.New("exit status 1")
	if state, _ := appState("", failed); state != "unknown (exit status 1)" {
		t.Fatalf("silent failure read as %q", state)
	}
	if state, _ := appState("No service\n", failed); state != "" {
		t.Fatalf("No service read as %q", state)
	}
	if state, iface := appState("Connected\n  InterfaceName : utun7\n", nil); state != "Connected" || iface != "utun7" {
		t.Fatalf("got %q/%q", state, iface)
	}
}

func TestParseServiceReadsTheJobNotItsSections(t *testing.T) {
	out := "system/com.vybava.vpn.lovinka-admin = {\n\tactive count = 1\n\tpath = (submitted by launchctl[48754])\n\ttype = Submitted\n\tstate = running\n\n\tsockets = {\n\t\t\ttype = stream\n\t\tstate = active\n\t}\n\tpid = 48756\n}\n"
	got := ParseService(out)
	want := Service{Loaded: true, Type: "Submitted", Path: "(submitted by launchctl[48754])", Running: true, PID: 48756}
	if got != want {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

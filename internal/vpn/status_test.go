package vpn

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

type fakeMac struct {
	app    string
	svc    Service
	files  map[string]bool
	route  string
	dnsErr error
}

func (f fakeMac) AppState(context.Context, string) string          { return f.app }
func (f fakeMac) Service(context.Context, string) (Service, error) { return f.svc, nil }
func (f fakeMac) Exists(path string) bool                          { return f.files[path] }
func (f fakeMac) RouteInterface(context.Context, string) string    { return f.route }
func (f fakeMac) AskDNS(context.Context, string) error             { return f.dnsErr }
func (f fakeMac) Dial(_ context.Context, addr string) error        { return nil }

func TestInspectClassifiesByRouteNotByAppState(t *testing.T) {
	wgUp := map[string]bool{"/var/run/wireguard/lovinka-admin.name": true, "/var/run/wireguard/utun11.sock": true}
	installed := map[string]bool{"/var/run/wireguard/lovinka-admin.name": true, "/var/run/wireguard/utun11.sock": true, PlistPath("lovinka-admin"): true}
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
			name:  "WireGuard.app carries it",
			mac:   fakeMac{app: "Connected", route: "utun4"},
			state: "up", summary: "up via WireGuard.app",
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

func TestParseServiceReadsTheJobNotItsSections(t *testing.T) {
	out := "system/com.vybava.vpn.lovinka-admin = {\n\tactive count = 1\n\tpath = (submitted by launchctl[48754])\n\ttype = Submitted\n\tstate = running\n\n\tsockets = {\n\t\t\ttype = stream\n\t\tstate = active\n\t}\n\tpid = 48756\n}\n"
	got := ParseService(out)
	want := Service{Loaded: true, Type: "Submitted", Path: "(submitted by launchctl[48754])", Running: true, PID: 48756}
	if got != want {
		t.Fatalf("got %#v, want %#v", got, want)
	}
}

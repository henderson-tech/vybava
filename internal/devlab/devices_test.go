package devlab

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestScanRecordedLab(t *testing.T) {
	tl := newTestLab(t)
	tl.recordIOS(t)
	tl.recordAndroid(t)
	tl.seed(t, map[string]*Device{"s20": s20Row()})
	res, err := tl.Scan(context.Background(), ScanOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rows := res.Data.(ScanData).Devices
	byHandle := map[string]ScanRow{}
	for _, r := range rows {
		byHandle[orElse(r.UDID, r.Serial)] = r
		if strings.Contains(r.ProductType, "Watch") {
			t.Fatalf("watches are not lab devices: %+v", r)
		}
	}
	if len(rows) != 8 {
		t.Fatalf("want 7 iOS rows (watches dropped) + the S20, got %d", len(rows))
	}
	cases := []struct {
		handle string
		check  func(ScanRow) bool
	}{
		{iphone11UDID, func(r ScanRow) bool {
			return r.CoreDeviceID == iphone11Core && r.Model == "iPhone 11" && r.OS == "18.7.8" && r.Transport == "wired" && r.Online && *r.InstrumentsOnline && r.Suggested == "iphone11"
		}},
		{airUDID, func(r ScanRow) bool { return r.Online && !*r.InstrumentsOnline && r.Model == "iPhone Air" }},
		{"00008130-001645280421401C", func(r ScanRow) bool { return !r.Online && r.Paired }},
		{s20Serial, func(r ScanRow) bool {
			return r.InLedger == "s20" && r.Model == "SM-G980F" && r.OS == "13" && r.SDK == 33 && r.Suggested == ""
		}},
	}
	for _, tc := range cases {
		if r, ok := byHandle[tc.handle]; !ok || !tc.check(r) {
			t.Errorf("%s: %+v", tc.handle, r)
		}
	}
	if !slices.Contains(res.Next, "perflab device add iphone11 --udid "+iphone11UDID+" --json") {
		t.Fatalf("next %v", res.Next)
	}
	for _, n := range res.Next {
		if strings.Contains(n, s20Serial) {
			t.Fatalf("a registered device gets no add line: %s", n)
		}
	}
}

func TestAmbiguousNamesOnlyOnline(t *testing.T) {
	rows := []ScanRow{
		{Name: "Lab - iPhone", UDID: "A", Online: true},
		{Name: "Lab - iPhone", UDID: "B", Online: true},
		{Name: "Owner - iPhone", UDID: "C", Online: true},
		{Name: "Owner - iPhone", UDID: "D"},
	}
	got := ambiguousNames(rows)
	if len(got) != 1 || got[0].Code != DiagDeviceAmbiguous || !strings.Contains(got[0].Detail, "A, B") {
		t.Fatalf("%+v", got)
	}
}

func TestAdd(t *testing.T) {
	yes := true
	cases := []struct {
		name    string
		seed    map[string]*Device
		id      string
		opts    AddOptions
		wantErr string
		check   func(Device) bool
	}{
		{name: "iOS by udid fills identity from devicectl", id: "iphone11", opts: AddOptions{UDID: iphone11UDID, ExpectHz: 60},
			check: func(d Device) bool {
				return d.Platform == PlatformIOS && d.CoreDeviceID == iphone11Core && d.ProductType == "iPhone12,1" && d.OS == "18.7.8" && d.Transport == "wired" && d.ExpectHz == 60
			}},
		{name: "Android by serial fills identity from getprop", id: "s20", opts: AddOptions{Serial: s20Serial, ExpectHz: 120, ProtectPackages: []string{"app.fixit.client"}, Personal: &yes},
			check: func(d Device) bool {
				return d.Model == "SM-G980F" && d.OS == "13" && d.SDK == 33 && d.Resolution == "1080x2400" && d.Label == "Samsung SM-G980F" && d.Personal && d.Protects("app.fixit.client")
			}},
		{name: "re-run updates without duplicating", seed: map[string]*Device{"s20": s20Row()}, id: "s20", opts: AddOptions{Serial: s20Serial, ExpectHz: 60, ProtectPackages: []string{"app.fixit.client"}},
			check: func(d Device) bool {
				return d.ExpectHz == 60 && len(d.ProtectedPackages) == 1 && d.Label == "Samsung S20"
			}},
		{name: "alias owned by another id", seed: map[string]*Device{"s20": s20Row()}, id: "phone", opts: AddOptions{Serial: s20Serial}, wantErr: DiagDeviceExists},
		{name: "id names another device", seed: map[string]*Device{"s20": s20Row()}, id: "s20", opts: AddOptions{Serial: "R58M000000"}, wantErr: DiagDeviceExists},
		{name: "udid and serial", id: "x", opts: AddOptions{UDID: iphone11UDID, Serial: s20Serial}, wantErr: DiagUsage},
		{name: "bad id", id: "S20", opts: AddOptions{Serial: s20Serial}, wantErr: DiagUsage},
		{name: "odd refresh", id: "s20", opts: AddOptions{Serial: s20Serial, ExpectHz: 75}, wantErr: DiagUsage},
		{name: "iOS phone devicectl does not know", id: "iphone8", opts: AddOptions{UDID: "00008000-000000000000000A"}, wantErr: DiagDeviceOffline},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tl := newTestLab(t)
			tl.recordIOS(t)
			tl.recordAndroid(t)
			if tc.seed != nil {
				tl.seed(t, tc.seed)
			}
			res, err := tl.Add(context.Background(), tc.id, tc.opts)
			if code(err) != tc.wantErr {
				t.Fatalf("add: %v, want %q", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			led, _ := tl.LoadLedger()
			if d := led.Devices[tc.id]; d == nil || !tc.check(*d) {
				t.Fatalf("row %+v", d)
			}
			if res.Next[0] != "perflab device probe "+tc.id+" --json" {
				t.Fatalf("next %v", res.Next)
			}
		})
	}
}

func TestResolveNeverByName(t *testing.T) {
	led := &Ledger{Devices: map[string]*Device{"iphone11": iphone11Row(), "s20": s20Row()}}
	for handle, want := range map[string]string{
		"iphone11":                    "iphone11",
		strings.ToLower(iphone11UDID): "iphone11",
		iphone11Core:                  "iphone11",
		strings.ToLower(s20Serial):    "s20",
		"iPhone 11":                   "",
		"Owner - iPhone":              "",
		"":                            "",
	} {
		id, _, _ := led.Resolve(handle)
		if id != want {
			t.Errorf("Resolve(%q) = %q, want %q", handle, id, want)
		}
	}
}

func TestRemoveRefusedWhileLeased(t *testing.T) {
	tl := newTestLab(t)
	tl.seed(t, map[string]*Device{"s20": s20Row()})
	tl.as("session-one", claudePID)
	token := tl.acquire(t, "s20", time.Hour)
	if _, err := tl.Remove("s20", RemoveOptions{}); code(err) != DiagUsage {
		t.Fatalf("without --yes: %v", err)
	}
	if _, err := tl.Remove("s20", RemoveOptions{Yes: true}); code(err) != DiagDeviceLeased {
		t.Fatalf("while leased: %v", err)
	}
	if _, err := tl.ReleaseLease("s20", token); err != nil {
		t.Fatal(err)
	}
	if _, err := tl.Remove("s20", RemoveOptions{Yes: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := tl.Show("s20"); code(err) != DiagDeviceUnknown {
		t.Fatalf("after remove: %v", err)
	}
}

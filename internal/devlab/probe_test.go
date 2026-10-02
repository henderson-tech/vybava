package devlab

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestReadersOnRecordedOutput(t *testing.T) {
	if got := parseVsyncPeriod(fixture(t, "sf-latency.txt")); got != 8333333 {
		t.Errorf("vsync period %d, want 8333333 (120 Hz)", got)
	}
	if m := parseActiveMode(fixture(t, "dumpsys-display.txt")); m == nil || m.Width != 1080 || m.Height != 2400 || m.RefreshHz != 120 {
		t.Errorf("active mode %+v", m)
	}
	fallback := strings.ReplaceAll(fixture(t, "dumpsys-display.txt"), "mActiveSfDisplayMode=", "mPreviousSfDisplayMode=")
	if m := parseActiveMode(fallback); m == nil || m.Width != 1080 || m.RefreshHz != 120 {
		t.Errorf("framework mode-id fallback %+v", m)
	}
	if p := parsePower(fixture(t, "dumpsys-power.txt")); p.wakefulness != "Awake" || !p.stayOn || p.screenOffMs != 1800000 {
		t.Errorf("power %+v", p)
	}
	if kg, focus := parseWindow(fixture(t, "dumpsys-window.txt")); kg == nil || *kg || focus != "android" {
		t.Errorf("window keyguard=%v focus=%q", kg, focus)
	}
	if th := parseThermal(fixture(t, "dumpsys-thermal.txt")); th.Status != 0 || th.TempsC["AP"] != 32.0 || th.TempsC["SKIN"] != 32.1 {
		t.Errorf("thermal %+v", th)
	}
	if b := parseBattery(fixture(t, "dumpsys-battery.txt")); b.Level != 100 || !b.Powered || b.TempC != 31.7 {
		t.Errorf("battery %+v", b)
	}
	if m := parseMeminfo(fixture(t, "meminfo.txt")); m.TotalKB != 7623228 || m.AvailableKB != 3523640 || m.SwapTotalKB != 4194300 || m.SwapFreeKB != 3249484 {
		t.Errorf("meminfo %+v", m)
	}
	if p := parseGetprop(fixture(t, "getprop.txt")); p["ro.product.model"] != "SM-G980F" || p["ro.build.version.sdk"] != "33" || p["ro.product.marketname"] != "" {
		t.Errorf("getprop %v", p)
	}
	if got := parseWmSize(fixture(t, "wm-size.txt")); got != "1080x2400" {
		t.Errorf("wm size %q", got)
	}
	adb := parseAdbDevices(fixture(t, "adb-devices.txt") + "emulator-5554\tdevice product:sdk_gphone64 model:sdk transport_id:3\n")
	if len(adb) != 1 || adb[0].Serial != s20Serial || adb[0].State != "device" || adb[0].Model != "SM_G980F" || adb[0].Transport != "usb:32-3.3" {
		t.Errorf("adb devices %+v", adb)
	}
	xt := parseXctraceDevices(fixture(t, "xctrace-devices.txt"))
	if _, ok := xt.Online[iphone11UDID]; !ok {
		t.Errorf("iPhone 11 must be online: %v", xt.Online)
	}
	if _, ok := xt.Offline[airUDID]; !ok {
		t.Errorf("the Air sleeps under Devices Offline: %v", xt.Offline)
	}
	reg, err := parseTunnelRegistry([]byte(fixture(t, "tunnels-air.json")))
	if err != nil {
		t.Fatal(err)
	}
	if tun, ok := reg.tunnelFor(strings.ToLower(airUDID)); !ok || tun.RSDPort != 63650 || tun.Address != "fdd7:dc03:bca5::1" {
		t.Errorf("air tunnel %+v", tun)
	}
	empty, err := parseTunnelRegistry([]byte(fixture(t, "tunnels-empty.json")))
	if _, ok := empty.tunnelFor(iphone11UDID); err != nil || ok {
		t.Errorf("an empty registry has no tunnel: %v", err)
	}
	if _, err := parseTunnelRegistry([]byte(`{"status":"ERROR"}`)); err == nil {
		t.Error("a non-OK registry is an error")
	}
}

// recordS20Probe installs every in-device reading of an S20 probe.
func (tl *testLab) recordS20Probe(t *testing.T) {
	tl.recordAndroid(t)
	sh := "adb -s " + s20Serial + " shell "
	tl.exec.outs[sh+"dumpsys SurfaceFlinger --latency"] = fakeOut{stdout: fixture(t, "sf-latency.txt")}
	tl.exec.outs[sh+"dumpsys display"] = fakeOut{stdout: fixture(t, "dumpsys-display.txt")}
	tl.exec.outs[sh+"settings get secure refresh_rate_mode"] = fakeOut{stdout: "2\n"}
	tl.exec.outs[sh+"dumpsys power"] = fakeOut{stdout: fixture(t, "dumpsys-power.txt")}
	tl.exec.outs[sh+"dumpsys window"] = fakeOut{stdout: fixture(t, "dumpsys-window.txt")}
	tl.exec.outs[sh+"dumpsys thermalservice"] = fakeOut{stdout: fixture(t, "dumpsys-thermal.txt")}
	tl.exec.outs[sh+"dumpsys battery"] = fakeOut{stdout: fixture(t, "dumpsys-battery.txt")}
	tl.exec.outs[sh+"cat /proc/meminfo"] = fakeOut{stdout: fixture(t, "meminfo.txt")}
}

func TestProbeAndroid(t *testing.T) {
	cases := []struct {
		name     string
		expectHz int
		edit     func(tl *testLab)
		want     []string // severity:code rows that must appear
		wantNone bool
	}{
		{name: "recorded S20 at 120 Hz is clean", expectHz: 120, wantNone: true},
		{name: "ledger expects 60", expectHz: 60, want: []string{"error:" + DiagRefreshRateMismatch}},
		{name: "asleep and short timeout", expectHz: 120, edit: func(tl *testLab) {
			k := "adb -s " + s20Serial + " shell dumpsys power"
			tl.exec.outs[k] = fakeOut{stdout: "  mWakefulness=Asleep\n  mStayOn=false\n  mScreenOffTimeoutSetting=30000\n"}
		}, want: []string{"error:" + DiagDeviceLocked, "warning:" + DiagAutoLockOn}},
		{name: "hot and draining", expectHz: 120, edit: func(tl *testLab) {
			sh := "adb -s " + s20Serial + " shell "
			tl.exec.outs[sh+"dumpsys thermalservice"] = fakeOut{stdout: "Thermal Status: 3\n"}
			tl.exec.outs[sh+"dumpsys battery"] = fakeOut{stdout: "  AC powered: false\n  USB powered: false\n  level: 12\n"}
		}, want: []string{"error:" + DiagThermalHot, "error:" + DiagLowBattery}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tl := newTestLab(t)
			row := s20Row()
			row.ExpectHz = tc.expectHz
			tl.seed(t, map[string]*Device{"s20": row})
			tl.recordS20Probe(t)
			if tc.edit != nil {
				tc.edit(tl)
			}
			tl.as("session-one", claudePID)
			token := tl.acquire(t, "s20", time.Hour)
			res, err := tl.Probe(context.Background(), "s20", ProbeOptions{Lease: token})
			if err != nil {
				t.Fatal(err)
			}
			st := res.Data.(ProbeState)
			if !st.Deep || !st.Online || st.RefreshHz != 120 || st.RefreshSource != "surfaceflinger" || st.RefreshRateMode != "2" {
				t.Fatalf("state %+v", st)
			}
			got := codes(res.Diagnostics)
			if tc.wantNone && len(got) != 0 {
				t.Fatalf("diagnostics %v", got)
			}
			for _, w := range tc.want {
				if !strings.Contains(strings.Join(got, " "), w) {
					t.Fatalf("want %s in %v", w, got)
				}
			}
			for _, d := range res.Diagnostics {
				if strings.Contains(d.Fix, "perflab device shell") && !strings.Contains(d.Fix, "--lease "+token) {
					t.Fatalf("a fix must carry the holder's token: %s", d.Fix)
				}
			}
			led, _ := tl.LoadLedger()
			if lp := led.Devices["s20"].LastProbe; lp == nil || lp.RefreshHz != 120 || !lp.Online {
				t.Fatalf("lastProbe %+v", lp)
			}
		})
	}
}

// Without a lease, a device another holder leases is probed only from the
// host: no adb shell runs, so a measurement in progress is never loaded.
func TestProbeLeasedByOtherStaysOffDevice(t *testing.T) {
	tl := newTestLab(t)
	tl.seed(t, map[string]*Device{"s20": s20Row()})
	tl.recordS20Probe(t)
	tl.as("session-holder", claudePID)
	tl.acquire(t, "s20", time.Hour)
	tl.as("session-curious", claudePID)
	res, err := tl.Probe(context.Background(), "s20", ProbeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	st := res.Data.(ProbeState)
	if st.Deep || !st.Online || !strings.Contains(st.LeasedBy, "session-") {
		t.Fatalf("state %+v", st)
	}
	if tl.exec.called("adb -s " + s20Serial + " shell dumpsys") {
		t.Fatal("no in-device reading may run on a device another holder leases")
	}
	if !hasCode(res.Diagnostics, "info", DiagDeviceLeased) {
		t.Fatalf("diagnostics %v", codes(res.Diagnostics))
	}
}

func TestProbeIPhone11(t *testing.T) {
	tl := newTestLab(t)
	tl.seed(t, map[string]*Device{"iphone11": iphone11Row()})
	tl.recordIOS(t)
	tl.exec.outs["xcrun devicectl device info lockState --device "+iphone11Core] = fakeOut{json: fixture(t, "devicectl-lockstate.json")}
	tl.exec.outs["xcrun devicectl device info displays --device "+iphone11Core] = fakeOut{json: fixture(t, "devicectl-displays.json")}
	res, err := tl.Probe(context.Background(), "iphone11", ProbeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	st := res.Data.(ProbeState)
	if !st.Deep || !st.Online || !st.Paired || st.DeveloperMode != "enabled" || st.Locked == nil || *st.Locked || st.ScreenOn == nil || !*st.ScreenOn {
		t.Fatalf("state %+v", st)
	}
	if st.RefreshHz != 60 || st.RefreshSource != "ledger" || st.Tunnel != "missing" || st.InstrumentsOnline == nil || !*st.InstrumentsOnline {
		t.Fatalf("refresh/tunnel/instruments %+v", st)
	}
	humans := 0
	for _, d := range res.Diagnostics {
		if d.Code == DiagHumanCheck {
			humans++
		}
		if d.Severity == "error" {
			t.Fatalf("unexpected error %+v", d)
		}
	}
	if humans != len(iosHumanChecks) || !hasCode(res.Diagnostics, "warning", DiagTunnelMissing) {
		t.Fatalf("diagnostics %v", codes(res.Diagnostics))
	}

	tl.tunnels = ""
	res, _ = tl.Probe(context.Background(), "iphone11", ProbeOptions{})
	if !hasCode(res.Diagnostics, "warning", DiagTunnelRegistryDown) {
		t.Fatalf("registry down: %v", codes(res.Diagnostics))
	}

	// Seen live on 2026-10-02: backlight activeOn and unlocked, yet xctrace
	// listed the phone under Devices Offline. That is Instruments unable to
	// attach, not a sleeping screen.
	offline := strings.Replace(fixture(t, "xctrace-devices.txt"), "Owner - iPhone (18.7.8) ("+iphone11UDID+")\n", "", 1) +
		"Owner - iPhone (18.7.8) (" + iphone11UDID + ")\n"
	tl.exec.outs["xcrun xctrace list devices"] = fakeOut{stdout: offline}
	res, _ = tl.Probe(context.Background(), "iphone11", ProbeOptions{})
	if !hasCode(res.Diagnostics, "warning", DiagDeviceOffline) || hasCode(res.Diagnostics, "warning", DiagDeviceLocked) {
		t.Fatalf("instruments offline with the screen on: %v", codes(res.Diagnostics))
	}
}

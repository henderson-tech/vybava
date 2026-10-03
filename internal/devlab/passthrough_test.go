package devlab

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestShellArgv(t *testing.T) {
	s20, ip := s20Row(), iphone11Row()
	ip.ProtectedPackages = []string{"app.fixit.client"}
	cases := []struct {
		name    string
		dev     *Device
		args    []string
		want    string
		wantErr string
	}{
		{name: "adb shell", dev: s20, args: []string{"shell", "dumpsys", "gfxinfo", "app.fixit.client.dev", "framestats"}, want: "adb -s RF8N21PY1BF shell dumpsys gfxinfo app.fixit.client.dev framestats"},
		{name: "adb reverse", dev: s20, args: []string{"reverse", "tcp:23936", "tcp:23936"}, want: "adb -s RF8N21PY1BF reverse tcp:23936 tcp:23936"},
		{name: "leading adb", dev: s20, args: []string{"adb", "shell", "ls"}, wantErr: DiagUsage},
		{name: "another serial", dev: s20, args: []string{"-s", "R58M000000", "shell", "ls"}, wantErr: DiagUsage},
		{name: "kill-server drops every phone", dev: s20, args: []string{"kill-server"}, wantErr: DiagUsage},
		{name: "pm clear protected", dev: s20, args: []string{"shell", "pm", "clear", "app.fixit.client"}, wantErr: DiagPackageProtected},
		{name: "quoted pm uninstall protected", dev: s20, args: []string{"shell", "pm uninstall app.fixit.client"}, wantErr: DiagPackageProtected},
		{name: "adb uninstall protected", dev: s20, args: []string{"uninstall", "app.fixit.client"}, wantErr: DiagPackageProtected},
		{name: "nested quotes around the protected package", dev: s20, args: []string{"shell", "pm clear 'app.fixit.client'"}, wantErr: DiagPackageProtected},
		{name: "protected package glued to an operator", dev: s20, args: []string{"shell", "pm clear app.fixit.client;echo done"}, wantErr: DiagPackageProtected},
		{name: "clear the dev package", dev: s20, args: []string{"shell", "pm", "clear", "app.fixit.client.dev"}, want: "adb -s RF8N21PY1BF shell pm clear app.fixit.client.dev"},
		{name: "devicectl launch: --device after the verb", dev: ip, args: []string{"device", "process", "launch", "--terminate-existing", "app.fixit.client.dev"}, want: "xcrun devicectl device process launch --device " + iphone11Core + " --terminate-existing app.fixit.client.dev"},
		{name: "devicectl leaf verb", dev: ip, args: []string{"device", "reboot"}, want: "xcrun devicectl device reboot --device " + iphone11Core},
		{name: "devicectl already names the device", dev: ip, args: []string{"device", "info", "lockState", "--device", iphone11UDID}, want: "xcrun devicectl device info lockState --device " + iphone11UDID},
		{name: "devicectl names another device", dev: ip, args: []string{"device", "info", "lockState", "--device", airCore}, wantErr: DiagUsage},
		{name: "devicectl uninstall protected", dev: ip, args: []string{"device", "uninstall", "app", "app.fixit.client"}, wantErr: DiagPackageProtected},
		{name: "leading xcrun devicectl", dev: ip, args: []string{"xcrun", "devicectl", "device", "info", "apps"}, wantErr: DiagUsage},
		{name: "not device-scoped", dev: ip, args: []string{"list", "devices"}, wantErr: DiagUsage},
		{name: "unknown group", dev: ip, args: []string{"device", "screenshot", "x"}, wantErr: DiagUsage},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			argv, err := shellArgv("dev", tc.dev, "plt_x", tc.args)
			if code(err) != tc.wantErr {
				t.Fatalf("err %v, want %q", err, tc.wantErr)
			}
			if err == nil && strings.Join(argv, " ") != tc.want {
				t.Fatalf("argv\n got %s\nwant %s", strings.Join(argv, " "), tc.want)
			}
		})
	}
}

func TestShellUnderLease(t *testing.T) {
	tl := newTestLab(t)
	tl.seed(t, map[string]*Device{"s20": s20Row()})
	tl.exec.outs["adb -s "+s20Serial+" shell getprop ro.build.version.release"] = fakeOut{stdout: "13\n"}
	tl.exec.outs["adb -s "+s20Serial+" shell false"] = fakeOut{code: 1, stderr: "boom\n"}
	ctx := context.Background()
	if _, err := tl.Shell(ctx, "s20", "", []string{"shell", "ls"}, ShellOptions{}); code(err) != DiagLeaseRequired {
		t.Fatalf("no token: %v", err)
	}
	tl.as("session-one", claudePID)
	token := tl.acquire(t, "s20", time.Hour)
	res, err := tl.Shell(ctx, "s20", token, []string{"shell", "getprop", "ro.build.version.release"}, ShellOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if d := res.Data.(CommandData); d.Exit != 0 || d.Stdout != "13\n" || len(res.Diagnostics) != 0 {
		t.Fatalf("data %+v diags %v", d, codes(res.Diagnostics))
	}
	res, err = tl.Shell(ctx, "s20", token, []string{"shell", "false"}, ShellOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if d := res.Data.(CommandData); d.Exit != 1 || !hasCode(res.Diagnostics, "error", DiagDeviceCommandFailed) {
		t.Fatalf("a failing command is data.exit + DEVICE_COMMAND_FAILED: %+v %v", d, codes(res.Diagnostics))
	}
	// The fix re-runs the passthrough: the raw `adb -s <serial>` it wraps is
	// refused by claude-guards on a leased phone.
	if fix := res.Diagnostics[0].Fix; fix != "perflab device shell s20 --lease "+token+" --json -- shell false" {
		t.Fatalf("fix %q", fix)
	}
	// A streaming command is capped while it streams, not after.
	tl.exec.outs["adb -s "+s20Serial+" logcat"] = fakeOut{stdout: strings.Repeat("x", maxCapturedStdout+10)}
	res, err = tl.Shell(ctx, "s20", token, []string{"logcat"}, ShellOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if d := res.Data.(CommandData); len(d.Stdout) != maxCapturedStdout || !d.StdoutTruncated {
		t.Fatalf("captured %d bytes, truncated %v", len(d.Stdout), d.StdoutTruncated)
	}
}

func TestScreencapAndPull(t *testing.T) {
	tl := newTestLab(t)
	ip := iphone11Row()
	tl.seed(t, map[string]*Device{"s20": s20Row(), "iphone11": ip})
	tl.as("session-one", claudePID)
	s20Token := tl.acquire(t, "s20", time.Hour)
	ipToken := tl.acquire(t, "iphone11", time.Hour)
	dir := t.TempDir()
	ctx := context.Background()

	tl.exec.outs["adb -s "+s20Serial+" exec-out screencap -p"] = fakeOut{stdout: string(pngMagic) + "pixels"}
	shot := filepath.Join(dir, "s20.png")
	res, err := tl.Screencap(ctx, "s20", s20Token, ScreencapOptions{Out: shot})
	if err != nil || len(res.Diagnostics) != 0 || res.Data.(CommandData).Bytes != int64(len(pngMagic)+6) {
		t.Fatalf("screencap: %v %v %+v", err, codes(res.Diagnostics), res.Data)
	}
	tl.exec.outs["adb -s "+s20Serial+" exec-out screencap -p"] = fakeOut{stdout: "error: closed"}
	res, _ = tl.Screencap(ctx, "s20", s20Token, ScreencapOptions{Out: shot})
	if !hasCode(res.Diagnostics, "error", DiagDeviceCommandFailed) {
		t.Fatalf("a non-PNG screencap fails: %v", codes(res.Diagnostics))
	}
	if _, err := tl.Screencap(ctx, "iphone11", ipToken, ScreencapOptions{Out: shot}); code(err) != DiagTunnelMissing {
		t.Fatalf("iOS 18 screencap without a tunnel: %v", err)
	}
	tl.tunnels = fixture(t, "tunnels-air.json")
	iosShot := filepath.Join(dir, "ip.png")
	key := "ios screenshot --udid " + iphone11UDID + " --address fdcc:a1a5:582b::1 --rsd-port 55936 --output " + iosShot
	tl.exec.outs[key] = fakeOut{}
	if err := os.WriteFile(iosShot, append(append([]byte{}, pngMagic...), 'x'), 0o644); err != nil {
		t.Fatal(err)
	}
	if res, err := tl.Screencap(ctx, "iphone11", ipToken, ScreencapOptions{Out: iosShot}); err != nil || len(res.Diagnostics) != 0 {
		t.Fatalf("iOS screencap over the registry tunnel: %v %v", err, codes(res.Diagnostics))
	}

	if _, err := tl.Pull(ctx, "iphone11", ipToken, PullOptions{Remote: "Documents/mmkv/x", Local: filepath.Join(dir, "x")}); code(err) != DiagUsage {
		t.Fatalf("appDataContainer without a bundle id: %v", err)
	}
	local := filepath.Join(dir, "mmkv")
	tl.exec.outs["xcrun devicectl device copy from --device "+iphone11Core+" --domain-type appDataContainer --domain-identifier app.fixit.client.dev --source Documents/mmkv/x --destination "+local] = fakeOut{}
	if _, err := tl.Pull(ctx, "iphone11", ipToken, PullOptions{Remote: "Documents/mmkv/x", Local: local, DomainID: "app.fixit.client.dev"}); err != nil {
		t.Fatal(err)
	}
	tl.exec.outs["adb -s "+s20Serial+" pull /sdcard/trace.pftrace "+local] = fakeOut{}
	if _, err := tl.Pull(ctx, "s20", s20Token, PullOptions{Remote: "/sdcard/trace.pftrace", Local: local}); err != nil {
		t.Fatal(err)
	}
}

// RealExec's stderr keeps only the tail every diagnostic reads.
func TestTailBufferKeepsTheTail(t *testing.T) {
	tb := &tailBuffer{max: 8}
	for _, chunk := range []string{"abcdef", "ghijkl", "mnopqrstuvwxyz", "0123"} {
		_, _ = tb.Write([]byte(chunk))
	}
	if got := tb.String(); got != "wxyz0123" || len(tb.b) >= 2*tb.max {
		t.Fatalf("tail %q, kept %d bytes", got, len(tb.b))
	}
}

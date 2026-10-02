package buildindex

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var s20 = Device{ID: "s20", Platform: "android", Serial: "RF8N21PY1BF", ProtectedPackages: []string{"app.fixit.client"}}

const devicePath = "/data/app/~~0A0iAOuiHkSi_S8WQ9uXDg==/app.fixit.client.dev-2FFA4BiiX_Wb0nQX1yxElg==/base.apk"

func androidItem(t *testing.T, pkg string) Installable {
	t.Helper()
	apk := filepath.Join(t.TempDir(), "app-variant.apk")
	if err := os.WriteFile(apk, []byte("variant apk"), 0o644); err != nil {
		t.Fatal(err)
	}
	sum, _ := fileSHA256(apk)
	return Installable{ID: "pf1-01234567-0123456789ab", Platform: "android", AppID: pkg, Path: apk,
		Stamp: InstalledStamp{Platform: "android", AppID: pkg, APKSHA256: sum}}
}

func TestInstallAndroid(t *testing.T) {
	cases := []struct {
		name          string
		pkg           string
		script        func(f *fakeRunner, sum string) *fakeRunner
		code          string
		uninstalled   bool
		mustNotCalled string
	}{
		{name: "replaces in place", pkg: "app.fixit.client.dev", script: func(f *fakeRunner, sum string) *fakeRunner {
			return f.on("install -r", ok("Performing Streamed Install\nSuccess\n"))
		}},
		{name: "a key change of an unprotected package uninstalls first", pkg: "app.fixit.client.dev", uninstalled: true, script: func(f *fakeRunner, sum string) *fakeRunner {
			return f.once("install -r", Result{Exit: 1, Stderr: []byte("adb: failed to install x.apk: Failure [INSTALL_FAILED_UPDATE_INCOMPATIBLE: Existing package app.fixit.client.dev signatures do not match newer version; ignoring!]\n")}).
				on("uninstall app.fixit.client.dev", ok("Success\n")).
				on("install -r", ok("Success\n"))
		}},
		{name: "a protected package is never uninstalled", pkg: "app.fixit.client", code: DiagPackageProtected, mustNotCalled: "RF8N21PY1BF uninstall", script: func(f *fakeRunner, sum string) *fakeRunner {
			return f.on("install -r", Result{Exit: 1, Stdout: []byte("Failure [INSTALL_FAILED_VERSION_DOWNGRADE: Downgrade detected: Update version code 1 is older than current 2]\n")})
		}},
		{name: "a dropped cable is transport", pkg: "app.fixit.client.dev", code: DiagInstallTransport, script: func(f *fakeRunner, sum string) *fakeRunner {
			return f.on("install -r", Result{Exit: 1, Stderr: []byte("adb: device 'RF8N21PY1BF' not found\n")})
		}},
		{name: "another rejection names its reason", pkg: "app.fixit.client.dev", code: DiagInstallFailed, script: func(f *fakeRunner, sum string) *fakeRunner {
			return f.on("install -r", Result{Exit: 1, Stdout: []byte("Failure [INSTALL_FAILED_INSUFFICIENT_STORAGE]\n")})
		}},
		{name: "the device keeps another APK", pkg: "app.fixit.client.dev", code: DiagInstallFailed, script: func(f *fakeRunner, sum string) *fakeRunner {
			return f.on("install -r", ok("Success\n")).
				on("sha256sum", ok("9e0440b0ddbee7ccf37f8bd655724dd411a68a779fa9e7fa13439fc00e43d668  "+devicePath+"\n"))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			it := androidItem(t, tc.pkg)
			f := tc.script(newFake(t), it.Stamp.APKSHA256).
				on("cmd package compile -m speed -f", ok("Success\n")).
				on("pm path", ok("package:"+devicePath+"\n")).
				on("sha256sum", ok(it.Stamp.APKSHA256+"  "+devicePath+"\n"))
			res, err := Install(context.Background(), f, InstallSpec{Item: it, Device: s20})
			if tc.code != "" {
				wantCode(t, err, tc.code)
			} else if err != nil {
				t.Fatal(err)
			} else if !res.Compiled || res.Stamp.APKSHA256 != it.Stamp.APKSHA256 {
				t.Fatalf("result = %+v", res)
			}
			if res.Uninstalled != tc.uninstalled {
				t.Fatalf("uninstalled = %v, want %v", res.Uninstalled, tc.uninstalled)
			}
			if tc.mustNotCalled != "" && f.called(tc.mustNotCalled) {
				t.Fatalf("%s ran: %v", tc.mustNotCalled, f.calls)
			}
		})
	}
}

func TestInstallIOSLaunchesAndChecksTheStamp(t *testing.T) {
	apps, err := os.ReadFile(filepath.Join("testdata", "devicectl-info-apps.json"))
	if err != nil {
		t.Fatal(err)
	}
	iphone11 := Device{ID: "iphone11", Platform: "ios", CoreDeviceID: "F8C49F38-CB34-5B6D-9D34-07F194198EEA"}
	answerApps := func(body []byte) func(c Cmd) Result {
		return func(c Cmd) Result {
			for i, a := range c.Argv {
				if a == "--json-output" {
					_ = os.WriteFile(c.Argv[i+1], body, 0o644)
				}
			}
			return ok("")
		}
	}
	item := Installable{ID: "pf1-aabbccdd-0123456789ab", Platform: "ios", AppID: "app.fixit.client", Path: "/cache/variants/x/FixIt.app",
		Stamp: InstalledStamp{Platform: "ios", AppID: "app.fixit.client", BundleVersion: "1"}}
	f := newFake(t).
		on("devicectl device install app --device F8C49F38", ok("App installed:\n")).
		on("process launch --device F8C49F38-CB34-5B6D-9D34-07F194198EEA --terminate-existing app.fixit.client", ok("Launched application\n")).
		onDo("device info apps", answerApps(apps))
	res, err := Install(context.Background(), f, InstallSpec{Item: item, Device: iphone11})
	if err != nil || !res.Launched {
		t.Fatalf("install: %v %+v", err, res)
	}
	item.Stamp.BundleVersion = "1.4242"
	_, err = Install(context.Background(), newFake(t).
		on("devicectl device install app", ok("")).
		on("process launch", ok("")).
		onDo("device info apps", answerApps(apps)), InstallSpec{Item: item, Device: iphone11})
	wantCode(t, err, DiagInstallFailed)
	_, err = Install(context.Background(), newFake(t).
		on("devicectl device install app", ok("")).
		on("process launch", Result{Exit: 1, Stderr: []byte("ERROR: The request to open \"app.fixit.client\" failed. The device was not, or could not be, unlocked.\n")}),
		InstallSpec{Item: item, Device: iphone11})
	wantCode(t, err, DiagDeviceLocked)
}

func TestVerifyInstalledFences(t *testing.T) {
	apps, _ := os.ReadFile(filepath.Join("testdata", "devicectl-info-apps.json"))
	iphone11 := Device{ID: "iphone11", Platform: "ios", CoreDeviceID: "F8C49F38-CB34-5B6D-9D34-07F194198EEA"}
	appsRunner := func(body []byte) *fakeRunner {
		return newFake(t).onDo("device info apps", func(c Cmd) Result {
			_ = os.WriteFile(c.Argv[len(c.Argv)-2], body, 0o644)
			return ok("")
		})
	}
	if err := VerifyInstalled(context.Background(), appsRunner(apps), iphone11, InstalledStamp{Platform: "ios", AppID: "app.fixit.client", BundleVersion: "1"}, "v1"); err != nil {
		t.Fatal(err)
	}
	err := VerifyInstalled(context.Background(), appsRunner(apps), iphone11, InstalledStamp{Platform: "ios", AppID: "app.fixit.client", BundleVersion: "1.987"}, "pf1-aabbccdd-0123456789ab")
	d := wantCode(t, err, DiagDeviceStateChanged)
	if !strings.Contains(d.Diag.Fix, "perflab install pf1-aabbccdd-0123456789ab --device iphone11") {
		t.Fatalf("fix = %s", d.Diag.Fix)
	}
	err = VerifyInstalled(context.Background(), appsRunner([]byte(`{"result":{"apps":[]}}`)), iphone11, InstalledStamp{Platform: "ios", AppID: "app.fixit.client", BundleVersion: "1"}, "v1")
	wantCode(t, err, DiagAppNotInstalled)

	want := "f6678eab12e1916cae290323c30630b3861f752d4edc2088ec76c3af4fe02e45"
	android := newFake(t).on("pm path", ok("package:"+devicePath+"\n")).on("sha256sum", ok("9e0440b0ddbee7ccf37f8bd655724dd411a68a779fa9e7fa13439fc00e43d668  "+devicePath+"\n"))
	err = VerifyInstalled(context.Background(), android, s20, InstalledStamp{Platform: "android", AppID: "app.fixit.client.dev", APKSHA256: want}, "v2")
	wantCode(t, err, DiagDeviceStateChanged)
	err = VerifyInstalled(context.Background(), newFake(t).on("pm path", Result{Exit: 1}), s20, InstalledStamp{Platform: "android", AppID: "app.fixit.client.dev", APKSHA256: want}, "v2")
	wantCode(t, err, DiagAppNotInstalled)
}

func TestBundleVersionStampStaysNumeric(t *testing.T) {
	for _, original := range []string{"1", "42", "3.1.7", "", "beta"} {
		got := bundleVersionStamp(original, "pf1-aabbccdd-0123456789ab")
		parts := strings.Split(got, ".")
		if len(parts) != 2 {
			t.Fatalf("%q -> %q", original, got)
		}
		for _, p := range parts {
			for _, r := range p {
				if r < '0' || r > '9' {
					t.Fatalf("%q -> %q is not numeric", original, got)
				}
			}
		}
	}
	if bundleVersionStamp("1", "a") == bundleVersionStamp("1", "b") {
		t.Fatal("two variants share a stamp")
	}
}

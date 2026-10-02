package claudeguards

import (
	"strings"
	"testing"
	"time"

	"github.com/henderson-tech/vybava/internal/devlab"
)

func TestGuardDeviceLeased(t *testing.T) {
	s20 := devlab.HeldLease{DeviceID: "s20", Platform: devlab.PlatformAndroid, Aliases: []string{"s20", "RF8N21PY1BF"},
		Holder: &devlab.Owner{Worktree: "marketplace-ui-vt-4229", Branch: "work/marketplace-ui-vt-4229"}, ExpiresAt: time.Now().Add(time.Hour)}
	iphone11 := devlab.HeldLease{DeviceID: "iphone11", Platform: devlab.PlatformIOS,
		Aliases: []string{"iphone11", "00008030-001E6D961122802E", "F8C49F38-CB34-5B6D-9D34-07F194198EEA"}}
	both := []devlab.HeldLease{s20, iphone11}
	cases := []struct {
		name   string
		held   []devlab.HeldLease
		cmd    string
		denied bool
	}{
		{"adb names the leased serial", both, "adb -s RF8N21PY1BF shell dumpsys window", true},
		{"devicectl names the CoreDevice id", both, "xcrun devicectl device info apps --device F8C49F38-CB34-5B6D-9D34-07F194198EEA --json-output /tmp/a.json", true},
		{"xctrace names the UDID, any case", both, "xcrun xctrace record --device 00008030-001e6d961122802e --attach 12 --time-limit 30s", true},
		{"appium env names the UDID", both, "FIXIT_APPIUM_IOS_UDID=00008030-001E6D961122802E bun run appium:e2e:ios", true},
		{"ANDROID_SERIAL on any command", both, "ANDROID_SERIAL=RF8N21PY1BF ./gradlew installRelease", true},
		{"go-ios --udid=", both, "ios screenshot --udid=00008030-001E6D961122802E --output x.png", true},
		{"behind a runner", both, "timeout 30 adb -s RF8N21PY1BF logcat -d", true},
		{"in a chain", both, "cd /tmp && adb -s RF8N21PY1BF pull /sdcard/x.png .", true},
		{"bare adb device verb", both, "adb shell input tap 540 210", true},
		{"adb kill-server", both, "adb kill-server", true},
		{"env runner names the UDID", both, "env FIXIT_APPIUM_IOS_UDID=00008030-001E6D961122802E bun run appium:e2e:ios", true},
		{"env runner names another serial", both, "env ANDROID_SERIAL=R58M12ABCDE adb shell getprop", false},
		{"adb -e reaches only an emulator", both, "adb -e shell getprop", false},
		{"adb devices", both, "adb devices -l", false},
		{"another serial", both, "adb -s R58M12ABCDE shell getprop", false},
		{"perflab passthrough", both, "perflab device shell s20 --lease plt_x -- shell dumpsys window", false},
		{"vybava perflab", both, "vybava perflab device shell RF8N21PY1BF --lease plt_x -- shell ls", false},
		{"perflab with the serial in env", both, "ANDROID_SERIAL=RF8N21PY1BF perflab doctor --json", false},
		{"a search for the serial", both, "rg -n RF8N21PY1BF ~/Exports/FixIt/perf", false},
		{"echoed text", both, "echo adb -s RF8N21PY1BF shell", false},
		{"devicectl list", both, "xcrun devicectl list devices", false},
		{"the ledger id alone", both, "adb -s s20 shell ls", false},
		{"nothing leased", nil, "adb -s RF8N21PY1BF shell ls", false},
		{"bare adb with only an iPhone leased", []devlab.HeldLease{iphone11}, "adb shell ls", false},
		{"remote payload runs elsewhere", both, "devbox run -- 'adb -s RF8N21PY1BF shell ls'", false},
	}
	orig := deviceLeases
	defer func() { deviceLeases = orig }()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			deviceLeases = func() []devlab.HeldLease { return c.held }
			in := &HookInput{}
			in.ToolInput.Command = c.cmd
			d := guardDeviceLeased(in)
			if (d != nil) != c.denied {
				t.Fatalf("%q: denied=%v, want %v", c.cmd, d != nil, c.denied)
			}
			if d != nil {
				if d.Rule != "machine:device-leased" || !strings.Contains(d.Message, "perflab device shell") || !strings.Contains(d.Message, "perflab lease status") {
					t.Fatalf("denial must name the passthrough and the status fix: %s", d.Text())
				}
			}
		})
	}
}

func TestGuardDeviceLeasedSkipsLeaseReadsWhenNothingTargetsADevice(t *testing.T) {
	orig := deviceLeases
	defer func() { deviceLeases = orig }()
	read := false
	deviceLeases = func() []devlab.HeldLease { read = true; return nil }
	in := &HookInput{}
	in.ToolInput.Command = "go test ./... && git status"
	if guardDeviceLeased(in) != nil || read {
		t.Fatalf("a command naming no device must not read the lease files (read=%v)", read)
	}
}

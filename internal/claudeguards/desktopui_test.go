package claudeguards

import "testing"

func TestDesktopUITestPatterns(t *testing.T) {
	block := []string{
		// the 2026-09-27 command that crashed Warp
		`cd macos && tuist generate --no-open >/tmp/t.log 2>&1 && nice -n 10 xcodebuild -workspace SwitcherooBar.xcworkspace -scheme SwitcherooBar -destination 'platform=macOS' build test >/tmp/x.log 2>&1; echo "exit=$?"`,
		"xcodebuild -scheme App test",
		"xcodebuild test -scheme App -destination 'platform=macOS,arch=arm64'",
		"xcodebuild test -scheme App -destination 'platform=macOS,variant=Mac Catalyst'",
		"xcodebuild test -scheme App -only-testing:AppUITests/MedalTests",
		"xcodebuild test -scheme App -only-testing:AppTests -only-testing:AppUITests",
		"xcodebuild test-without-building -xctestrun App.xctestrun",
		"xcrun xcodebuild -scheme App test",
		"timeout 600 xcodebuild -scheme App test",
		"tuist test",
		"tuist test App --platform macos",
		"tuist test --test-targets AppUITests",
	}
	pass := []string{
		"xcodebuild -scheme App build",
		"xcodebuild -scheme test build",
		"xcodebuild build-for-testing -scheme App",
		"xcodebuild test -scheme App -destination 'platform=macOS' -only-testing:SwitcherooBarTests",
		"xcodebuild test -scheme App -skip-testing:SwitcherooBarUITests",
		"xcodebuild test -scheme App -only-testing SwitcherooBarTests/TimesheetTests",
		"xcodebuild test -scheme App -destination 'platform=iOS Simulator,name=iPhone 17'",
		"xcodebuild test -scheme App -destination id=5E1F0A7C-2D3B-4C1A-9E8F-0123456789AB",
		"xcodebuild test -scheme App -sdk iphonesimulator",
		"tuist test --skip-ui-tests",
		"tuist test --platform ios",
		"tuist test -d 'iPhone 17'",
		"tuist test --test-targets AppTests",
		"tuist test --skip-test-targets AppUITests",
		"tuist generate --no-open",
		"devbox run -- 'xcodebuild -scheme App test'",
		"echo xcodebuild -scheme App test",
		"grep -rn 'xcodebuild test' docs/",
	}
	for _, c := range block {
		if desktopUITestMatch(c) == "" {
			t.Errorf("should block: %s", c)
		}
	}
	for _, c := range pass {
		if m := desktopUITestMatch(c); m != "" {
			t.Errorf("should pass: %s (matched %q)", c, m)
		}
	}
}

func TestDesktopUITestsEscape(t *testing.T) {
	in := &HookInput{}
	in.ToolInput.Command = "xcodebuild -scheme App test"
	if guardDesktopUITests(in) == nil {
		t.Fatal("a bare macOS test run must be denied")
	}
	in.ToolInput.Command = "CLAUDE_GUARDS_ALLOW_DESKTOP_UI=1 xcodebuild -scheme App test"
	if d := guardDesktopUITests(in); d != nil {
		t.Fatalf("the escape must pass, got %s", d.Rule)
	}
}

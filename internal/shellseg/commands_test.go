package shellseg

import (
	"reflect"
	"testing"
)

func TestLocalCommandsKeepAssignments(t *testing.T) {
	cases := []struct {
		cmd  string
		want []Command
	}{
		{"ANDROID_SERIAL=RF8N21PY1BF ./gradlew installRelease", []Command{{Text: "./gradlew installRelease", Assign: []string{"ANDROID_SERIAL=RF8N21PY1BF"}}}},
		{"A='x y' B=2 bun run e2e && adb devices", []Command{{Text: "bun run e2e", Assign: []string{"A=x y", "B=2"}}, {Text: "adb devices"}}},
		{"sh -c 'U=abc ios info'", []Command{{Text: "sh -c 'U=abc ios info'"}, {Text: "ios info", Assign: []string{"U=abc"}}}},
		{"devbox run -- 'S=1 adb shell ls'", nil},
	}
	for _, c := range cases {
		if got := LocalCommands(c.cmd); !reflect.DeepEqual(got, c.want) {
			t.Errorf("LocalCommands(%q) = %#v, want %#v", c.cmd, got, c.want)
		}
	}
}

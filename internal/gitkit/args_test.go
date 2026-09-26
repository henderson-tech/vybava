package gitkit

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

func TestVerbArgsParse(t *testing.T) {
	spec := verbArgs{values: []string{"repo", "every-seconds"}, bools: []string{"json"}, positionals: 1, usage: "usage: v <pr> [--repo P]"}
	for _, tc := range []struct {
		argv  []string
		flags map[string]string
		pos   []string
	}{
		{[]string{"104", "--repo", "/r"}, map[string]string{"repo": "/r"}, []string{"104"}},
		// a flag's value is never read as the positional, whatever the order
		{[]string{"--repo", "/r", "104"}, map[string]string{"repo": "/r"}, []string{"104"}},
		{[]string{"--repo=/r", "--json", "104"}, map[string]string{"repo": "/r", "json": ""}, []string{"104"}},
		{nil, map[string]string{}, nil},
		// --json is gitkit's persistent flag: every verb takes it, declared or not
		{[]string{"--json", "104"}, map[string]string{"json": ""}, []string{"104"}},
	} {
		flags, pos, err := spec.parse("v", tc.argv)
		if err != nil || !maps.Equal(flags, tc.flags) || !slices.Equal(pos, tc.pos) {
			t.Errorf("parse(%q) = %v %v %v", tc.argv, flags, pos, err)
		}
	}
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"--every", "60"}, "unknown argument --every"},
		{[]string{"104", "105"}, `unexpected argument "105"`},
		{[]string{"-r", "/x"}, "unknown argument -r"},
		{[]string{"--repo"}, "--repo needs a value"},
		{[]string{"--repo", "--json"}, "--repo needs a value"},
		{[]string{"--json=yes"}, "--json takes no value"},
		// an empty anchor never quietly means the cwd's repository
		{[]string{"--repo", ""}, "--repo needs a value"},
		{[]string{"--repo="}, "--repo needs a value"},
		// the old scanners kept the first --repo, parse would keep the last: refuse
		{[]string{"--repo", "/a", "--repo=/b"}, "--repo is given twice"},
	} {
		_, _, err := spec.parse("v", tc.argv)
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), spec.usage) {
			t.Errorf("parse(%q): err = %v, want %q + usage", tc.argv, err, tc.want)
		}
	}
	// -1 positionals: any number
	if _, pos, err := (verbArgs{positionals: -1}).parse("v", []string{"a", "b", "c"}); err != nil || len(pos) != 3 {
		t.Errorf("unbounded positionals: %v %v", pos, err)
	}
	if got := repoAnchor(map[string]string{"repo": "/r"}); !slices.Equal(got, []string{"--repo", "/r"}) {
		t.Errorf("repoAnchor = %q", got)
	}
	if got := repoAnchor(map[string]string{}); got != nil {
		t.Errorf("repoAnchor without --repo = %q, want nil (GIT_SKILL_REPO, then cwd)", got)
	}
}

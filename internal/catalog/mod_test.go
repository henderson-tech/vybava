package catalog

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestModRequiresManifestsNamedByItsID(t *testing.T) {
	t.Parallel()

	valid := fstest.MapFS{
		"mods/demo/.claude-plugin/plugin.json": {Data: []byte(`{"name":"demo"}`)},
		"mods/demo/hooks/hooks.json":           {Data: []byte(`{"modules":["./register.ts"]}`)},
	}
	item := Item{ID: "demo", Kind: KindMod, Source: "mods/demo"}
	if err := validateMod(valid, item); err != nil {
		t.Fatalf("validateMod() error = %v", err)
	}

	cases := map[string]struct {
		source fstest.MapFS
		item   Item
		want   string
	}{
		"source elsewhere": {valid, Item{ID: "demo", Kind: KindMod, Source: "skills/demo"}, "source must be mods/demo"},
		"no hooks.json": {fstest.MapFS{
			"mods/demo/.claude-plugin/plugin.json": {Data: []byte(`{"name":"demo"}`)},
		}, item, "hooks.json"},
		"manifest names another plugin": {fstest.MapFS{
			"mods/demo/.claude-plugin/plugin.json": {Data: []byte(`{"name":"other"}`)},
			"mods/demo/hooks/hooks.json":           {Data: []byte(`{}`)},
		}, item, `names "other"`},
	}
	for name, tc := range cases {
		if err := validateMod(tc.source, tc.item); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: validateMod() error = %v, want it to mention %q", name, err, tc.want)
		}
	}
}

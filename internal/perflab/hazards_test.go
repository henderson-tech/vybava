package perflab

import (
	"os"
	"path/filepath"
	"testing"
)

// The fixtures are trimmed shapes of the sites the calendar and hub work
// fixed (PR #1770, d1ac9d0b4a): one file per rule, plus a gated loop.
var hazardFixtures = map[string]string{
	"components/Pulse.tsx": `export function Pulse() {
  const v = useSharedValue(0);
  useEffect(() => { v.value = withRepeat(withTiming(1, { duration: 900 }), -1, true); }, []);
  return <Animated.View style={style} />;
}`,
	"components/GatedPulse.tsx": `export function GatedPulse({ shown }) {
  const ambient = useAmbientMotion(shown);
  useEffect(() => { if (ambient) v.value = withRepeat(withTiming(1), -1); }, [ambient]);
  return null;
}`,
	"components/Canvas.tsx": `export function Rok() {
  const clock = useClock();
  useFrameCallback(() => {});
  const p = usePathValue((path) => { 'worklet'; });
  return <Canvas />;
}`,
	"components/Layer.tsx": `export const Block = ({ layer }) => (
  <Animated.View renderToHardwareTextureAndroid={layer} style={s} />
);
export const Kept = ({ layer }) => (
  <Animated.View renderToHardwareTextureAndroid={layer} collapsable={!layer} />
);`,
	"components/List.tsx": `export const L = () => <FlatList removeClippedSubviews data={d} />;`,
	"app/(worker)/hub.tsx": `export default function Hub() {
  return <ScrollView>{rows}</ScrollView>;
}`,
	"app/(worker)/feed.tsx": `export default function Feed() {
  return <ScrollView><FlashList data={d} /></ScrollView>;
}`,
	"hooks/useMany.ts": `export const useMany = (ids) => useQueries({ queries: ids.map(q) });
export const useCombined = (ids) => useQueries({ queries: ids.map(q), combine: (r) => r });`,
	"components/Price.tsx": `const fmt = new Intl.NumberFormat('cs-CZ');
export function Price({ cents }) {
  const f = new Intl.NumberFormat('cs-CZ', { style: 'currency', currency: 'CZK' });
  const g = useMemo(() => new Intl.NumberFormat('cs-CZ'), []);
  return <Text>{f.format(cents / 100)}</Text>;
}`,
	"components/__tests__/Pulse.test.tsx": `withRepeat(x, -1)`,
	"node_modules/lib/index.ts":           `withRepeat(x, -1)`,
}

func writeHazardTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range hazardFixtures {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestHazards(t *testing.T) {
	root := writeHazardTree(t)
	tool := &Tool{ProjectDir: root, Config: &Config{App: AppConfig{Root: "."}, Hazards: &HazardsConfig{AmbientGates: []string{"useAmbientMotion"}, VisibilityHint: []string{"shown"}}}}
	res, err := tool.Hazards(HazardsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rows := res.Data.(map[string]any)["rows"].([]HazardRow)
	type key struct{ file, rule string }
	got := map[key]HazardRow{}
	for _, r := range rows {
		got[key{r.File, r.Rule}] = r
	}
	want := []struct {
		file, rule string
		gated      *bool
	}{
		{"components/Pulse.tsx", HazardInfiniteRepeat, ptrBool(false)},
		{"components/GatedPulse.tsx", HazardInfiniteRepeat, ptrBool(true)},
		{"components/Canvas.tsx", HazardClock, ptrBool(false)},
		{"components/Canvas.tsx", HazardFrameCallback, ptrBool(false)},
		{"components/Canvas.tsx", HazardPathValue, ptrBool(false)},
		{"components/Layer.tsx", HazardTextureNoCollap, nil},
		{"components/List.tsx", HazardClippedSubviews, nil},
		{"app/(worker)/hub.tsx", HazardScrollViewRoute, nil},
		{"hooks/useMany.ts", HazardQueriesNoComb, nil},
		{"components/Price.tsx", HazardIntlInRender, nil},
	}
	for _, w := range want {
		r, ok := got[key{w.file, w.rule}]
		if !ok {
			t.Errorf("missing %s in %s", w.rule, w.file)
			continue
		}
		if (w.gated == nil) != (r.Gated == nil) || (w.gated != nil && *w.gated != *r.Gated) {
			t.Errorf("%s %s gated = %v, want %v", w.file, w.rule, r.Gated, w.gated)
		}
	}
	if len(rows) != len(want) {
		t.Errorf("%d rows, want %d (one Kept layer, the FlashList route, the combined query, the memoised and module Intl and the test/node_modules files are not sites): %+v", len(rows), len(want), rows)
	}
	if g := got[key{"components/GatedPulse.tsx", HazardInfiniteRepeat}]; len(g.Hints) != 1 || g.Hints[0] != "shown" {
		t.Errorf("a gated loop lists the visibility hints found: %+v", g.Hints)
	}

	base := filepath.Join(t.TempDir(), "hazards.json")
	if _, err := tool.Hazards(HazardsOptions{WriteBaseline: base}); err != nil {
		t.Fatal(err)
	}
	res, err = tool.Hazards(HazardsOptions{Gate: true, Baseline: base})
	if err != nil || len(res.Diagnostics) != 0 {
		t.Fatalf("a tree equal to its baseline passes the gate: %v %+v", err, res.Diagnostics)
	}
	if err := os.WriteFile(filepath.Join(root, "components/New.tsx"), []byte("useFrameCallback(() => {})"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, _ = tool.Hazards(HazardsOptions{Gate: true, Baseline: base})
	if len(res.Diagnostics) != 1 || res.Diagnostics[0].Code != DiagHazardNew || res.Diagnostics[0].Severity != "error" {
		t.Fatalf("a new site fails the gate with HAZARD_NEW: %+v", res.Diagnostics)
	}
}

func ptrBool(b bool) *bool { return &b }

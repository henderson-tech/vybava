package polishkit

import (
	"fmt"
	"net/url"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/henderson-tech/vybava/internal/vconfig"
)

// Config is the `polish` section of vybava.config.ts. Its TypeScript twin is
// PolishConfig in internal/vconfig/config-helpers.ts; change both together.
type Config struct {
	// Targets maps a target to repository-relative globs; a changed file maps
	// to the first target (app, ui, api order) whose glob matches.
	Targets map[Target][]string `json:"targets"`
	// Base is the diff base ref (default origin/main, then the default branch).
	Base string `json:"base,omitempty"`
	// Out is the gitignored run root (default .polish): <out>/pass-<n>.
	Out     string   `json:"out,omitempty"`
	Lanes   []Lane   `json:"lanes"`
	Screens []Screen `json:"screens,omitempty"`
}

// Target is one axis of the polish verb.
type Target string

const (
	TargetApp Target = "app"
	TargetUI  Target = "ui"
	TargetAPI Target = "api"
)

// Targets is the fixed order targets are matched and listed in.
var Targets = []Target{TargetApp, TargetUI, TargetAPI}

// LaneKind is what a lane resolves to.
type LaneKind string

const (
	KindIOSSim          LaneKind = "ios-sim"
	KindIOSDevice       LaneKind = "ios-device"
	KindAndroidDevice   LaneKind = "android-device"
	KindAndroidEmulator LaneKind = "android-emulator"
	KindBrowser         LaneKind = "browser"
	KindServer          LaneKind = "server"
)

// LaneKinds is the closed kind vocabulary.
var LaneKinds = []LaneKind{KindIOSSim, KindIOSDevice, KindAndroidDevice, KindAndroidEmulator, KindBrowser, KindServer}

// Lane is one device or URL a target is polished on.
type Lane struct {
	ID     string   `json:"id"`
	Target Target   `json:"target"`
	Kind   LaneKind `json:"kind"`
	// Runtime is the iOS runtime version prefix an ios-sim lane needs ('18.6', '26').
	Runtime string `json:"runtime,omitempty"`
	// DeviceType is the simulator device type name or the emulator AVD name.
	DeviceType string `json:"deviceType,omitempty"`
	// Device is a devicectl name/udid (ios-device) or an adb serial (android-device).
	Device string `json:"device,omitempty"`
	// Nav lists the Android navigation modes to shoot (default gesture).
	Nav    []string `json:"nav,omitempty"`
	Themes []string `json:"themes,omitempty"`
	// TextSizes are content-size names (iOS) or font-scale numbers as strings (Android) shot besides the default.
	TextSizes []string `json:"textSizes,omitempty"`
	Locale    string   `json:"locale,omitempty"`
	// URL is a browser lane's base URL or a server lane's health URL.
	URL string `json:"url,omitempty"`
}

// Screen is one screen a chrome cell shoots.
type Screen struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Target Target `json:"target"`
	// URL is a deep link the app opens (app) or a path under the lane URL (ui).
	URL      string `json:"url"`
	SettleMs int    `json:"settleMs,omitempty"`
	Area     string `json:"area,omitempty"`
}

// Defaults.
const (
	DefaultBase     = "origin/main"
	DefaultOut      = ".polish"
	DefaultSettleMs = 1500
)

// Vocabularies.
var (
	Themes      = []string{"light", "dark"}
	NavModes    = []string{"gesture", "3button"}
	Intensities = []string{"quick", "default", "full"}
	kebab       = regexp.MustCompile(`^[a-z0-9]+([.-][a-z0-9]+)*$`)
)

// IsAndroid reports the kinds that take a nav mode.
func (k LaneKind) IsAndroid() bool { return k == KindAndroidDevice || k == KindAndroidEmulator }

// IsDevice reports the kinds shoot can capture natively.
func (k LaneKind) IsShootable() bool {
	return k == KindIOSSim || k == KindAndroidDevice || k == KindAndroidEmulator
}

// WithDefaults fills every optional knob.
func (c Config) WithDefaults() Config {
	if c.Base == "" {
		c.Base = DefaultBase
	}
	if c.Out == "" {
		c.Out = DefaultOut
	}
	lanes := make([]Lane, len(c.Lanes))
	for i, l := range c.Lanes {
		if len(l.Themes) == 0 {
			l.Themes = slices.Clone(Themes)
		}
		if l.Kind.IsAndroid() && len(l.Nav) == 0 {
			l.Nav = []string{"gesture"}
		}
		lanes[i] = l
	}
	c.Lanes = lanes
	screens := make([]Screen, len(c.Screens))
	for i, s := range c.Screens {
		if s.SettleMs == 0 {
			s.SettleMs = DefaultSettleMs
		}
		screens[i] = s
	}
	c.Screens = screens
	return c
}

// Validate reports every problem at once.
func (c Config) Validate() []string {
	var problems []string
	add := func(s string) {
		if s != "" {
			problems = append(problems, s)
		}
	}
	if len(c.Targets) == 0 {
		add("targets must map at least one of app, ui, api to globs")
	}
	for target, globs := range c.Targets {
		if !slices.Contains(Targets, target) {
			add(fmt.Sprintf("targets.%s: unknown target (app, ui, api)", target))
		}
		if len(globs) == 0 {
			add(fmt.Sprintf("targets.%s must list at least one glob", target))
		}
		for _, g := range globs {
			if strings.TrimSpace(g) == "" || path.IsAbs(g) {
				add(fmt.Sprintf("targets.%s: glob %q must be repository-relative", target, g))
			}
		}
	}
	if c.Out != "" {
		if path.IsAbs(c.Out) || strings.HasPrefix(c.Out, "~") || strings.HasPrefix(path.Clean(c.Out), "..") || path.Clean(c.Out) == "." {
			add(fmt.Sprintf("out %q must be repo-relative and name a directory", c.Out))
		}
	}
	if len(c.Lanes) == 0 {
		add("lanes must list at least one lane")
	}
	seen := map[string]bool{}
	for _, l := range c.Lanes {
		if !kebab.MatchString(l.ID) {
			add(fmt.Sprintf("lane id %q is not kebab-case", l.ID))
		}
		if seen[l.ID] {
			add(fmt.Sprintf("lane %q is listed twice", l.ID))
		}
		seen[l.ID] = true
		if !slices.Contains(Targets, l.Target) {
			add(fmt.Sprintf("lane %s: target %q is not app, ui or api", l.ID, l.Target))
		}
		if !slices.Contains(LaneKinds, l.Kind) {
			add(fmt.Sprintf("lane %s: kind %q is not one of %s", l.ID, l.Kind, joinKinds()))
		}
		switch l.Kind {
		case KindIOSSim:
			if l.Runtime == "" || l.DeviceType == "" {
				add(fmt.Sprintf("lane %s: an ios-sim lane needs runtime and deviceType", l.ID))
			}
		case KindAndroidEmulator:
			if l.DeviceType == "" {
				add(fmt.Sprintf("lane %s: an android-emulator lane needs deviceType (the AVD name)", l.ID))
			}
		case KindBrowser, KindServer:
			if u, err := url.Parse(l.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				add(fmt.Sprintf("lane %s: a %s lane needs an http(s) url", l.ID, l.Kind))
			}
		}
		for _, n := range l.Nav {
			if !slices.Contains(NavModes, n) {
				add(fmt.Sprintf("lane %s: nav %q is not gesture or 3button", l.ID, n))
			}
		}
		if len(l.Nav) > 0 && !l.Kind.IsAndroid() {
			add(fmt.Sprintf("lane %s: nav applies to android lanes only", l.ID))
		}
		for _, th := range l.Themes {
			if !slices.Contains(Themes, th) {
				add(fmt.Sprintf("lane %s: theme %q is not light or dark", l.ID, th))
			}
		}
		for _, ts := range l.TextSizes {
			if l.Kind.IsAndroid() {
				if _, err := strconv.ParseFloat(ts, 64); err != nil {
					add(fmt.Sprintf("lane %s: android textSizes are font_scale numbers as strings, not %q", l.ID, ts))
				}
			} else if strings.TrimSpace(ts) == "" {
				add(fmt.Sprintf("lane %s: textSizes holds an empty name", l.ID))
			}
		}
	}
	seenScreen := map[string]bool{}
	for _, s := range c.Screens {
		if !kebab.MatchString(s.ID) {
			add(fmt.Sprintf("screen id %q is not kebab-case", s.ID))
		}
		if seenScreen[s.ID] {
			add(fmt.Sprintf("screen %q is listed twice", s.ID))
		}
		seenScreen[s.ID] = true
		if s.Target != TargetApp && s.Target != TargetUI {
			add(fmt.Sprintf("screen %s: target %q is not app or ui", s.ID, s.Target))
		}
		if strings.TrimSpace(s.URL) == "" {
			add(fmt.Sprintf("screen %s: url is required", s.ID))
		}
		if s.SettleMs < 0 {
			add(fmt.Sprintf("screen %s: settleMs must not be negative", s.ID))
		}
	}
	return problems
}

func joinKinds() string {
	parts := make([]string, len(LaneKinds))
	for i, k := range LaneKinds {
		parts[i] = string(k)
	}
	return strings.Join(parts, ", ")
}

// Lane looks a lane up by id.
func (c Config) Lane(id string) (Lane, bool) {
	for _, l := range c.Lanes {
		if l.ID == id {
			return l, true
		}
	}
	return Lane{}, false
}

// Screen looks a screen up by id.
func (c Config) Screen(id string) (Screen, bool) {
	for _, s := range c.Screens {
		if s.ID == id {
			return s, true
		}
	}
	return Screen{}, false
}

// LaneIDs lists the ids of the lanes of the given targets (all when empty),
// in config order.
func (c Config) LaneIDs(targets []Target) []string {
	var ids []string
	for _, l := range c.Lanes {
		if len(targets) == 0 || slices.Contains(targets, l.Target) {
			ids = append(ids, l.ID)
		}
	}
	return ids
}

// TargetOf maps a changed path to its target, in fixed target order.
func (c Config) TargetOf(file string) (Target, bool) {
	for _, target := range Targets {
		for _, g := range c.Targets[target] {
			if vconfig.MatchPath(g, file) {
				return target, true
			}
		}
	}
	return "", false
}

// globRoot is the literal prefix of a glob: the directory it is rooted in.
func globRoot(glob string) string {
	var root []string
	for _, part := range strings.Split(glob, "/") {
		if strings.ContainsAny(part, "*?[") {
			break
		}
		root = append(root, part)
	}
	return strings.Join(root, "/")
}

// TargetOfDir maps a repo-relative directory to the target whose glob roots it.
func (c Config) TargetOfDir(dir string) (Target, bool) {
	dir = strings.Trim(path.Clean(dir), "/")
	if dir == "." || dir == "" {
		return "", false
	}
	for _, target := range Targets {
		for _, g := range c.Targets[target] {
			root := globRoot(g)
			if root != "" && (dir == root || strings.HasPrefix(dir, root+"/")) {
				return target, true
			}
		}
	}
	return "", false
}

// itoa avoids a strconv import in every file.
func itoa(n int) string { return strconv.Itoa(n) }

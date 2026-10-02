package perflab

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Section is the vybava.config.ts key perflab reads.
const Section = "perflab"

// Config is the `perflab` section of a project's vybava.config.ts: the
// adapter that tells the engine where the Expo app lives and which project
// commands build, run and reset it. Go twin of PerflabConfig in
// internal/vconfig/config-helpers.ts; change both together. Unknown keys are
// rejected (vconfig.Section); unknown {tokens} by Validate.
type Config struct {
	App         AppConfig                `json:"app"`
	Profiles    map[string]ProfileConfig `json:"profiles"`
	Fingerprint *FingerprintConfig       `json:"fingerprint,omitempty"`
	Build       *BuildConfig             `json:"build,omitempty"`
	Scenarios   string                   `json:"scenarios"`
	Runner      RunnerConfig             `json:"runner"`
	API         *APIConfig               `json:"api,omitempty"`
	Hooks       *HooksConfig             `json:"hooks,omitempty"`
	Out         string                   `json:"out,omitempty"`
	Hazards     *HazardsConfig           `json:"hazards,omitempty"`
}

// AppConfig is the Expo app inside the repo.
type AppConfig struct {
	Root    string            `json:"root"`
	Entry   string            `json:"entry,omitempty"`
	IOS     *IOSAppConfig     `json:"ios,omitempty"`
	Android *AndroidAppConfig `json:"android,omitempty"`
}

// IOSAppConfig is app.ios.
type IOSAppConfig struct {
	Scheme   string `json:"scheme"`
	BundleID string `json:"bundleId"`
	Team     string `json:"team,omitempty"`
}

// AndroidAppConfig is app.android.
type AndroidAppConfig struct {
	Package  string `json:"package"`
	Activity string `json:"activity,omitempty"`
}

// ProfileConfig is one build profile: a command printing KEY=VALUE lines.
type ProfileConfig struct {
	Env string `json:"env"`
}

// FingerprintConfig overrides the Expo fingerprint command.
type FingerprintConfig struct {
	Cmd               string   `json:"cmd,omitempty"`
	ExtraNativeInputs []string `json:"extraNativeInputs,omitempty"`
}

// BuildConfig holds the project-specific build recipes.
type BuildConfig struct {
	IOSBundled string `json:"iosBundled,omitempty"`
	Android    string `json:"android,omitempty"`
}

// RunnerConfig is the project's scenario runner.
type RunnerConfig struct {
	Cmd             string            `json:"cmd"`
	Env             map[string]string `json:"env,omitempty"`
	Unset           []string          `json:"unset,omitempty"`
	AppiumHome      string            `json:"appiumHome,omitempty"`
	AppiumServerLog string            `json:"appiumServerLog,omitempty"`
}

// APIConfig is the backend the measured app talks to.
type APIConfig struct {
	WS     string           `json:"ws,omitempty"`
	Origin string           `json:"origin"`
	Hold   string           `json:"hold,omitempty"`
	Health string           `json:"health,omitempty"`
	Device *APIDeviceConfig `json:"device,omitempty"`
}

// APIDeviceConfig says how each platform reaches the API.
type APIDeviceConfig struct {
	Android *AndroidAPIConfig `json:"android,omitempty"`
	IOS     *IOSAPIConfig     `json:"ios,omitempty"`
}

// AndroidAPIConfig: the build bakes localhost:<devicePort>, adb reverse
// carries it to perflab's forwarder (the only strategy today: "reverse").
type AndroidAPIConfig struct {
	Strategy   string `json:"strategy"`
	DevicePort int    `json:"devicePort"`
}

// IOSAPIConfig: the bundle bakes an origin the phone reaches on its own
// (strategy "bake"); Origin is a command printing it.
type IOSAPIConfig struct {
	Strategy string `json:"strategy"`
	Origin   string `json:"origin,omitempty"`
}

// HooksConfig are the project commands `perflab app link|reset` run.
type HooksConfig struct {
	SignInLink string `json:"signInLink,omitempty"`
	ResetWorld string `json:"resetWorld,omitempty"`
}

// HazardsConfig tunes the static render-cost sweep.
type HazardsConfig struct {
	// AmbientGates are hook names that gate an infinite animation (a file
	// mentioning one counts its loops as gated).
	AmbientGates []string `json:"ambientGates,omitempty"`
	// VisibilityHint words, when present in a gated file, are listed so a
	// reviewer checks real visibility; the sweep never claims it.
	VisibilityHint []string `json:"visibilityHint,omitempty"`
	// Exclude are repository-relative globs left out of the sweep.
	Exclude []string `json:"exclude,omitempty"`
}

// Tokens is the closed engine token vocabulary every adapter command and
// env value may use. A token outside it, or outside the field's own subset,
// is CONFIG_INVALID.
var Tokens = []string{
	"account", "apiOrigin", "appPath", "appRoot", "branch", "branchSlug", "bundleId", "bundleSha",
	"coreDeviceId", "date", "deviceApiOrigin", "devicePort", "nativeKey", "outDir", "package",
	"platform", "profile", "route", "runDir", "scenario", "scenarios", "serial", "team",
	"timeoutMs", "topic", "udid", "variant", "wdaBundleId", "wdaDerivedData", "world", "ws",
}

// runTokens are the tokens a scenario run resolves (runner.cmd, runner.env).
var runTokens = []string{
	"apiOrigin", "appPath", "appRoot", "bundleId", "bundleSha", "coreDeviceId", "date",
	"deviceApiOrigin", "devicePort", "nativeKey", "package", "platform", "profile", "runDir",
	"scenario", "scenarios", "serial", "team", "timeoutMs", "topic", "udid", "variant",
	"wdaBundleId", "wdaDerivedData", "ws",
}

// fieldTokens maps each templated field to the tokens resolvable there.
func fieldTokens() map[string][]string {
	origin := []string{"ws"}
	return map[string][]string{
		"profiles.*.env":         {"platform", "profile", "apiOrigin", "deviceApiOrigin", "devicePort", "ws", "appRoot"},
		"fingerprint.cmd":        {"appRoot", "platform", "profile"},
		"build.iosBundled":       {"outDir", "platform", "profile", "appRoot", "team", "apiOrigin", "deviceApiOrigin", "devicePort", "ws"},
		"build.android":          {"outDir", "platform", "profile", "appRoot", "apiOrigin", "deviceApiOrigin", "devicePort", "ws"},
		"scenarios":              {"platform", "appRoot"},
		"runner.cmd":             runTokens,
		"runner.env.*":           runTokens,
		"api.ws":                 {"branch", "branchSlug"},
		"api.origin":             origin,
		"api.hold":               origin,
		"api.device.ios.origin":  origin,
		"hooks.signInLink":       {"account", "route", "platform", "apiOrigin", "deviceApiOrigin", "ws"},
		"hooks.resetWorld":       {"world", "scenario", "platform", "apiOrigin", "deviceApiOrigin", "ws"},
		"out":                    {"date", "topic"},
		"runner.appiumHome":      nil,
		"runner.appiumServerLog": nil,
	}
}

var tokenRe = regexp.MustCompile(`\{([A-Za-z][A-Za-z0-9_]*)\}`)

// TokensOf lists the {token} placeholders of a template. A shell parameter
// expansion (${VAR}) is not a token.
func TokensOf(value string) []string {
	var out []string
	for _, m := range tokenRe.FindAllStringSubmatchIndex(value, -1) {
		if m[0] > 0 && value[m[0]-1] == '$' {
			continue
		}
		out = append(out, value[m[2]:m[3]])
	}
	return out
}

var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Validate lists every problem of the section; empty means usable.
func (c Config) Validate() []string {
	var errs []string
	allowed := fieldTokens()
	check := func(field, key, value string) {
		for _, name := range TokensOf(value) {
			if !slices.Contains(Tokens, name) {
				errs = append(errs, fmt.Sprintf("%s: {%s} is not an engine token (known: %s)", field, name, braced(Tokens)))
				continue
			}
			if !slices.Contains(allowed[key], name) {
				errs = append(errs, fmt.Sprintf("%s: {%s} has no value there (allowed: %s)", field, name, orNone(braced(allowed[key]))))
			}
		}
	}
	if strings.TrimSpace(c.App.Root) == "" {
		errs = append(errs, "app.root is required (the Expo app's directory, relative to the repo root)")
	}
	if c.App.IOS == nil && c.App.Android == nil {
		errs = append(errs, "app needs ios and/or android")
	}
	if c.App.IOS != nil && (c.App.IOS.Scheme == "" || c.App.IOS.BundleID == "") {
		errs = append(errs, "app.ios needs scheme and bundleId")
	}
	if c.App.Android != nil && c.App.Android.Package == "" {
		errs = append(errs, "app.android needs package")
	}
	if len(c.Profiles) == 0 {
		errs = append(errs, "profiles needs at least one profile (e.g. perf: {env: '<command printing KEY=VALUE lines>'})")
	}
	for _, name := range sortedKeys(c.Profiles) {
		p := c.Profiles[name]
		if strings.TrimSpace(p.Env) == "" {
			errs = append(errs, fmt.Sprintf("profiles.%s.env is required", name))
		}
		check("profiles."+name+".env", "profiles.*.env", p.Env)
	}
	if c.Fingerprint != nil {
		check("fingerprint.cmd", "fingerprint.cmd", c.Fingerprint.Cmd)
		for i, ref := range c.Fingerprint.ExtraNativeInputs {
			if !strings.Contains(ref, "#/") {
				errs = append(errs, fmt.Sprintf("fingerprint.extraNativeInputs[%d] %q is not <file>#<json pointer>", i, ref))
			}
		}
	}
	if c.Build != nil {
		check("build.iosBundled", "build.iosBundled", c.Build.IOSBundled)
		check("build.android", "build.android", c.Build.Android)
		if c.Build.IOSBundled != "" && !slices.Contains(TokensOf(c.Build.IOSBundled), "outDir") {
			errs = append(errs, "build.iosBundled must write its .app into {outDir}")
		}
		if c.Build.Android != "" && !slices.Contains(TokensOf(c.Build.Android), "outDir") {
			errs = append(errs, "build.android must write its .apk into {outDir}")
		}
	}
	if strings.TrimSpace(c.Scenarios) == "" {
		errs = append(errs, "scenarios is required (a command printing the scenario rows as JSON)")
	}
	check("scenarios", "scenarios", c.Scenarios)
	if strings.TrimSpace(c.Runner.Cmd) == "" {
		errs = append(errs, "runner.cmd is required (the command that runs the measured scenarios)")
	}
	check("runner.cmd", "runner.cmd", c.Runner.Cmd)
	for _, name := range sortedKeys(c.Runner.Env) {
		if !envNameRe.MatchString(name) {
			errs = append(errs, fmt.Sprintf("runner.env: %q is not an environment variable name", name))
		}
		check("runner.env."+name, "runner.env.*", c.Runner.Env[name])
	}
	for _, name := range c.Runner.Unset {
		if !envNameRe.MatchString(name) {
			errs = append(errs, fmt.Sprintf("runner.unset: %q is not an environment variable name", name))
		}
	}
	check("runner.appiumHome", "runner.appiumHome", c.Runner.AppiumHome)
	check("runner.appiumServerLog", "runner.appiumServerLog", c.Runner.AppiumServerLog)
	if a := c.API; a != nil {
		check("api.ws", "api.ws", a.WS)
		check("api.origin", "api.origin", a.Origin)
		check("api.hold", "api.hold", a.Hold)
		if strings.TrimSpace(a.Origin) == "" {
			errs = append(errs, "api.origin is required (a command printing the host-side API origin)")
		}
		if a.Health != "" && !strings.HasPrefix(a.Health, "/") {
			errs = append(errs, "api.health must be a path starting with /")
		}
		if d := a.Device; d != nil {
			if d.Android != nil {
				if d.Android.Strategy != "reverse" {
					errs = append(errs, fmt.Sprintf("api.device.android.strategy %q: the only strategy is \"reverse\"", d.Android.Strategy))
				}
				if d.Android.DevicePort <= 0 || d.Android.DevicePort > 65535 {
					errs = append(errs, "api.device.android.devicePort must be a TCP port")
				}
			}
			if d.IOS != nil {
				if d.IOS.Strategy != "bake" {
					errs = append(errs, fmt.Sprintf("api.device.ios.strategy %q: the only strategy is \"bake\"", d.IOS.Strategy))
				}
				check("api.device.ios.origin", "api.device.ios.origin", d.IOS.Origin)
			}
		}
	}
	if h := c.Hooks; h != nil {
		check("hooks.signInLink", "hooks.signInLink", h.SignInLink)
		check("hooks.resetWorld", "hooks.resetWorld", h.ResetWorld)
	}
	check("out", "out", c.Out)
	return errs
}

func braced(tokens []string) string {
	out := make([]string, len(tokens))
	for i, t := range tokens {
		out[i] = "{" + t + "}"
	}
	return strings.Join(out, " ")
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Fields lists every templated field with its template, for `adapter
// check`'s resolved view. Empty templates are left out.
func (c Config) Fields() [][2]string {
	var out [][2]string
	add := func(field, value string) {
		if strings.TrimSpace(value) != "" {
			out = append(out, [2]string{field, value})
		}
	}
	for _, name := range sortedKeys(c.Profiles) {
		add("profiles."+name+".env", c.Profiles[name].Env)
	}
	if c.Fingerprint != nil {
		add("fingerprint.cmd", c.Fingerprint.Cmd)
	}
	if c.Build != nil {
		add("build.iosBundled", c.Build.IOSBundled)
		add("build.android", c.Build.Android)
	}
	add("scenarios", c.Scenarios)
	add("runner.cmd", c.Runner.Cmd)
	for _, name := range sortedKeys(c.Runner.Env) {
		add("runner.env."+name, c.Runner.Env[name])
	}
	if a := c.API; a != nil {
		add("api.ws", a.WS)
		add("api.origin", a.Origin)
		add("api.hold", a.Hold)
		if a.Device != nil && a.Device.IOS != nil {
			add("api.device.ios.origin", a.Device.IOS.Origin)
		}
	}
	if h := c.Hooks; h != nil {
		add("hooks.signInLink", h.SignInLink)
		add("hooks.resetWorld", h.ResetWorld)
	}
	add("out", c.Out)
	return out
}

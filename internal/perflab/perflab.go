// Package perflab is the verb layer of the perflab applet: physical-device
// performance testing of Expo / React Native apps. It owns no domain logic
// a sibling package already owns. internal/devlab keeps the device ledger and
// leases, buildindex the native builds, bundles and variants, doctor the
// preflights, netfwd the Android API forward, wda the prebuilt
// WebDriverAgent, analysis (over internal/xctrace and internal/framestats)
// every number. This package reads the project's adapter (the `perflab`
// section of vybava.config.ts), resolves its {token} templates, and composes
// those packages into the verbs that need several of them: run, probe,
// crashes, app, hazards, adapter check. Contract: docs/perflab.md.
package perflab

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/devlab"
	"github.com/henderson-tech/vybava/internal/perflab/buildindex"
	"github.com/henderson-tech/vybava/internal/perflab/hostexec"
	"github.com/henderson-tech/vybava/internal/shellword"
	"github.com/henderson-tech/vybava/internal/vconfig"
)

// Result is what every verb hands the envelope: Data for --json, Lines for a
// human, diagnostics and next commands (devlab's shape, shared).
type Result = devlab.Result

// Tool is one invocation's handle on the project and the lab.
type Tool struct {
	// ProjectDir is the adapter root: --project, else the git toplevel of
	// the working directory (a worktree resolves to its own root).
	ProjectDir string
	ConfigPath string
	// Config is the validated perflab section; nil when the project has none
	// (device and lease verbs need no adapter), with configErr saying why.
	Config    *Config
	configErr error

	Lab   *devlab.Lab
	Store buildindex.Store
	Exec  hostexec.Runner
	// Log receives the `perflab[...]` progress lines (stderr).
	Log    io.Writer
	Now    func() time.Time
	Getenv func(string) string
	// Sleep is time.Sleep (tests swap it).
	Sleep func(time.Duration)

	Branch string
	RepoID string
}

// Open resolves the project and loads its adapter section. A missing or
// invalid section is not an error here: verbs that need it call Cfg.
func Open(ctx context.Context, project string, log io.Writer) (*Tool, error) {
	if log == nil {
		log = io.Discard
	}
	dir := project
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		dir = wd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	lab, err := devlab.Open()
	if err != nil {
		return nil, err
	}
	dirs, err := buildindex.DefaultDirs()
	if err != nil {
		return nil, err
	}
	t := &Tool{
		ProjectDir: abs, Lab: lab, Store: buildindex.Store{Dirs: dirs}, Exec: hostexec.OS{},
		Log: log, Now: time.Now, Getenv: os.Getenv, Sleep: time.Sleep,
	}
	t.ProjectDir = t.gitOut(ctx, abs, "rev-parse", "--show-toplevel")
	if t.ProjectDir == "" {
		t.ProjectDir = abs
	}
	lab.ProjectDir = t.ProjectDir
	t.Branch = t.gitOut(ctx, t.ProjectDir, "rev-parse", "--abbrev-ref", "HEAD")
	if t.Branch == "" {
		// An unborn branch has no commit for rev-parse to name.
		t.Branch = t.gitOut(ctx, t.ProjectDir, "symbolic-ref", "--short", "HEAD")
	}
	t.RepoID = repoID(t.gitOut(ctx, t.ProjectDir, "remote", "get-url", "origin"))
	t.loadConfig()
	return t, nil
}

func (t *Tool) gitOut(ctx context.Context, dir string, args ...string) string {
	res, err := t.Exec.Run(ctx, hostexec.Cmd{Argv: append([]string{"git", "-C", dir}, args...), Timeout: 10 * time.Second})
	if err != nil || res.Exit != 0 {
		return ""
	}
	return strings.TrimSpace(string(res.Stdout))
}

var remoteRe = regexp.MustCompile(`[:/]([^/:]+/[^/]+?)(\.git)?$`)

// repoID is owner/name from a git remote URL, "" when there is none.
func repoID(url string) string {
	if m := remoteRe.FindStringSubmatch(strings.TrimSpace(url)); m != nil {
		return m[1]
	}
	return ""
}

const adapterFix = "perflab adapter check --json"

func (t *Tool) loadConfig() {
	cfg, err := vconfig.Load(t.ProjectDir)
	if err != nil {
		if errors.Is(err, vconfig.ErrNotFound) {
			t.configErr = diag(DiagConfigMissing, "no vybava.config.ts in "+t.ProjectDir+" or above; the perflab section (docs/perflab.md) tells the engine where the app is and how the project runs it",
				"vybava config init, add the perflab section, then "+adapterFix)
			return
		}
		t.configErr = diag(DiagConfigInvalid, "vybava.config.ts does not evaluate: "+err.Error(), "vybava config show --json, then "+adapterFix)
		return
	}
	t.ConfigPath = cfg.Path
	var c Config
	if err := cfg.Section(Section, &c); err != nil {
		if errors.Is(err, vconfig.ErrNoSection) {
			t.configErr = diag(DiagConfigMissing, cfg.Path+" has no perflab section", "add the perflab section (docs/perflab.md \"Adapter\"), then "+adapterFix)
			return
		}
		t.configErr = diag(DiagConfigInvalid, "perflab section: "+err.Error(), "fix the perflab section in "+cfg.Path+" (unknown keys are rejected), then "+adapterFix)
		return
	}
	if problems := c.Validate(); len(problems) > 0 {
		t.configErr = diag(DiagConfigInvalid, "perflab section: "+strings.Join(problems, "; "), "fix the perflab section in "+cfg.Path+", then "+adapterFix)
		return
	}
	t.Config = &c
}

// Cfg is the adapter section, or the CONFIG_MISSING / CONFIG_INVALID
// diagnostic a verb that needs it fails with.
func (t *Tool) Cfg() (*Config, error) {
	if t.Config == nil {
		if t.configErr == nil {
			return nil, diag(DiagConfigMissing, "no perflab section loaded", adapterFix)
		}
		return nil, t.configErr
	}
	return t.Config, nil
}

// progress is a `perflab[<tag>]` line writer on the log.
func (t *Tool) progress(tag string) *hostexec.Progress {
	return hostexec.NewProgress(t.Log, tag, t.Now)
}

// ---- token resolution -------------------------------------------------

// Vars resolves {token} values, lazily: an expensive one (an api.origin
// command) runs only when a template uses it, and at most once.
type Vars struct {
	fixed map[string]string
	lazy  map[string]func() (string, error)
	done  map[string]string
	// lists expand to one shell word per item in a command, and to the
	// items joined by commas in an env value ({scenarios}).
	lists map[string][]string
}

// SetList fixes a list token.
func (v *Vars) SetList(name string, items []string) {
	if v.lists == nil {
		v.lists = map[string][]string{}
	}
	v.lists[name] = items
	v.fixed[name] = strings.Join(items, ",")
}

// NewVars starts a resolver with fixed values.
func NewVars(fixed map[string]string) *Vars {
	v := &Vars{fixed: map[string]string{}, lazy: map[string]func() (string, error){}, done: map[string]string{}}
	for k, val := range fixed {
		v.fixed[k] = val
	}
	return v
}

// Set fixes a token's value.
func (v *Vars) Set(name, value string) { v.fixed[name] = value }

// Lazy registers a token computed on first use.
func (v *Vars) Lazy(name string, fn func() (string, error)) { v.lazy[name] = fn }

// Get resolves one token.
func (v *Vars) Get(name string) (string, bool, error) {
	if val, ok := v.fixed[name]; ok {
		return val, true, nil
	}
	if val, ok := v.done[name]; ok {
		return val, true, nil
	}
	fn, ok := v.lazy[name]
	if !ok {
		return "", false, nil
	}
	val, err := fn()
	if err != nil {
		return "", true, err
	}
	v.done[name] = val
	return val, true, nil
}

// Expand fills a template's tokens. In a shell command (quote) each value
// is one shell word; an env value takes the raw text. A token without a
// value in this context is CONFIG_INVALID naming the field.
func (v *Vars) Expand(field, tmpl string, quote bool) (string, error) {
	var b strings.Builder
	last := 0
	for _, m := range tokenRe.FindAllStringSubmatchIndex(tmpl, -1) {
		if m[0] > 0 && tmpl[m[0]-1] == '$' {
			continue
		}
		name := tmpl[m[2]:m[3]]
		val, ok, err := v.Get(name)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", diag(DiagConfigInvalid, fmt.Sprintf("%s: {%s} has no value for this verb", field, name), adapterFix)
		}
		b.WriteString(tmpl[last:m[0]])
		if items, isList := v.lists[name]; isList && quote {
			for i, item := range items {
				if i > 0 {
					b.WriteByte(' ')
				}
				b.WriteString(shellword.Quote(item))
			}
		} else if quote {
			b.WriteString(shellword.Quote(val))
		} else {
			b.WriteString(val)
		}
		last = m[1]
	}
	b.WriteString(tmpl[last:])
	return b.String(), nil
}

var slugRe = regexp.MustCompile(`[^A-Za-z0-9]+`)

// slug lowercases and joins the alphanumeric runs with '-'.
func slug(s string) string {
	return strings.Trim(strings.ToLower(slugRe.ReplaceAllString(s, "-")), "-")
}

// baseVars are the tokens every verb can resolve from the project alone,
// plus the API ones (lazy: they may run an adapter command).
func (t *Tool) baseVars(ctx context.Context, platform string) *Vars {
	v := NewVars(map[string]string{"date": t.Now().Format("2006-01-02")})
	if t.Branch != "" {
		// Outside git the branch tokens stay unset: a template using them
		// fails as CONFIG_INVALID instead of resolving to "".
		v.Set("branch", t.Branch)
		v.Set("branchSlug", slug(t.Branch))
	}
	if platform != "" {
		v.Set("platform", platform)
	}
	c := t.Config
	if c == nil {
		return v
	}
	v.Set("appRoot", c.App.Root)
	if c.App.IOS != nil {
		v.Set("bundleId", c.App.IOS.BundleID)
		if c.App.IOS.Team != "" {
			v.Set("team", c.App.IOS.Team)
		}
	}
	if c.App.Android != nil {
		v.Set("package", c.App.Android.Package)
	}
	if platform == "android" && c.App.Android != nil {
		v.Set("bundleId", c.App.Android.Package)
	}
	if a := c.API; a != nil {
		v.Lazy("ws", func() (string, error) {
			if a.WS == "" {
				return "", diag(DiagConfigInvalid, "api.ws is not set, yet a template uses {ws}", adapterFix)
			}
			return v.Expand("api.ws", a.WS, false)
		})
		v.Lazy("apiOrigin", func() (string, error) { return t.APIOrigin(ctx, v) })
		if a.Device != nil && a.Device.Android != nil {
			v.Set("devicePort", fmt.Sprint(a.Device.Android.DevicePort))
		}
		v.Lazy("deviceApiOrigin", func() (string, error) { return t.deviceAPIOrigin(ctx, v, platform) })
	}
	return v
}

// APIOrigin runs api.origin and returns its URL (the first http line).
func (t *Tool) APIOrigin(ctx context.Context, v *Vars) (string, error) {
	c := t.Config
	if c == nil || c.API == nil {
		return "", diag(DiagConfigInvalid, "the perflab section has no api, yet a template uses {apiOrigin}", adapterFix)
	}
	return t.originCommand(ctx, v, "api.origin", c.API.Origin)
}

func (t *Tool) originCommand(ctx context.Context, v *Vars, field, tmpl string) (string, error) {
	cmd, err := v.Expand(field, tmpl, true)
	if err != nil {
		return "", err
	}
	out, err := t.adapterCommand(ctx, field, cmd, nil, 60*time.Second)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://") {
			return strings.TrimRight(line, "/"), nil
		}
	}
	return "", diag(DiagAdapterCommandFailed, fmt.Sprintf("%s `%s` printed no http(s) URL", field, cmd), t.byHand(cmd))
}

// deviceAPIOrigin is what the build bakes for the phone: Android reverse
// strategy a loopback port the run forwards, iOS bake strategy the
// api.device.ios.origin command's URL.
func (t *Tool) deviceAPIOrigin(ctx context.Context, v *Vars, platform string) (string, error) {
	c := t.Config
	d := c.API.Device
	switch {
	case platform == "android" && d != nil && d.Android != nil:
		return fmt.Sprintf("http://localhost:%d", d.Android.DevicePort), nil
	case platform == "ios" && d != nil && d.IOS != nil && d.IOS.Origin != "":
		return t.originCommand(ctx, v, "api.device.ios.origin", d.IOS.Origin)
	case platform == "":
		return "", diag(DiagUsage, "{deviceApiOrigin} depends on the platform; pass --platform ios|android", "")
	}
	return t.APIOrigin(ctx, v)
}

// byHand is the fix line for an adapter command: run it from the root.
func (t *Tool) byHand(cmd string) string {
	return "(cd " + shellword.Quote(t.ProjectDir) + " && " + cmd + ")"
}

// adapterCommand runs one resolved adapter command with sh -c from the
// project root; a non-zero exit is ADAPTER_COMMAND_FAILED with the command
// to run by hand.
func (t *Tool) adapterCommand(ctx context.Context, field, cmd string, env []string, timeout time.Duration) (string, error) {
	res, err := t.Exec.Run(ctx, hostexec.Cmd{Argv: []string{"/bin/sh", "-c", cmd}, Dir: t.ProjectDir, Env: env, Timeout: timeout})
	if err != nil {
		return "", diag(DiagAdapterCommandFailed, fmt.Sprintf("%s `%s` could not start: %v", field, cmd, err), t.byHand(cmd))
	}
	if res.TimedOut {
		return "", diag(DiagAdapterCommandFailed, fmt.Sprintf("%s `%s` did not finish within %s", field, cmd, timeout), t.byHand(cmd))
	}
	if res.Exit != 0 {
		return "", diag(DiagAdapterCommandFailed, fmt.Sprintf("%s `%s` exited %d: %s", field, cmd, res.Exit, res.Tail()), t.byHand(cmd))
	}
	return string(res.Stdout), nil
}

// shellArgv wraps a resolved command line for buildindex (argv form).
func shellArgv(cmd string) []string {
	if strings.TrimSpace(cmd) == "" {
		return nil
	}
	return []string{"/bin/sh", "-c", cmd}
}

// Project is the buildindex view of the adapter for one platform and
// profile; {outDir} stays a token buildindex fills per build.
func (t *Tool) Project(ctx context.Context, platform, profile string) (buildindex.Project, *Vars, error) {
	c, err := t.Cfg()
	if err != nil {
		return buildindex.Project{}, nil, err
	}
	v := t.baseVars(ctx, platform)
	v.Set("profile", profile)
	v.Set("outDir", "{outDir}")
	p := buildindex.Project{
		ID: t.RepoID, RepoRoot: t.ProjectDir, AppRoot: c.App.Root, Entry: c.App.Entry,
	}
	if p.Entry == "" {
		p.Entry = "index.js"
	}
	if c.App.IOS != nil {
		p.IOS = buildindex.IOSApp{Scheme: c.App.IOS.Scheme, BundleID: c.App.IOS.BundleID, Team: c.App.IOS.Team}
	}
	if c.App.Android != nil {
		p.Android = buildindex.AndroidApp{Package: c.App.Android.Package, Activity: c.App.Android.Activity}
	}
	if f := c.Fingerprint; f != nil {
		p.ExtraNativeInputs = f.ExtraNativeInputs
		if f.Cmd != "" {
			cmd, err := v.Expand("fingerprint.cmd", f.Cmd, true)
			if err != nil {
				return p, v, err
			}
			p.FingerprintCmd = shellArgv(cmd)
		}
	}
	if b := c.Build; b != nil {
		if b.IOSBundled != "" && platform == "ios" {
			cmd, err := v.Expand("build.iosBundled", b.IOSBundled, false)
			if err != nil {
				return p, v, err
			}
			p.IOSBundledCmd = shellArgv(cmd)
		}
		if b.Android != "" && platform == "android" {
			cmd, err := v.Expand("build.android", b.Android, false)
			if err != nil {
				return p, v, err
			}
			p.AndroidBuildCmd = shellArgv(cmd)
		}
	}
	return p, v, nil
}

// ProfileEnv runs the profile's env command (KEY=VALUE lines).
func (t *Tool) ProfileEnv(ctx context.Context, v *Vars, profile string) (buildindex.ProfileEnv, error) {
	c, err := t.Cfg()
	if err != nil {
		return buildindex.ProfileEnv{}, err
	}
	p, ok := c.Profiles[profile]
	if !ok {
		return buildindex.ProfileEnv{}, diag(DiagUsage, fmt.Sprintf("profile %q is not in perflab.profiles (have: %s)", profile, strings.Join(sortedKeys(c.Profiles), ", ")),
			"pass --profile "+firstKey(c.Profiles))
	}
	cmd, err := v.Expand("profiles."+profile+".env", p.Env, true)
	if err != nil {
		return buildindex.ProfileEnv{}, err
	}
	return buildindex.LoadProfileEnv(ctx, t.Exec, shellArgv(cmd), t.ProjectDir)
}

func firstKey[V any](m map[string]V) string {
	keys := sortedKeys(m)
	if len(keys) == 0 {
		return "<profile>"
	}
	return keys[0]
}

// expandHome turns a leading ~ into the home directory.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

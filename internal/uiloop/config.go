package uiloop

import (
	"fmt"
	"maps"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Config is the `uiLoop` section of vybava.config.ts. Its TypeScript twin is
// UiLoopConfig in internal/vconfig/config-helpers.ts; change both together.
type Config struct {
	// Dir holds project.ts, screens/ and the synced vendor/ (repo-relative).
	Dir string `json:"dir"`
	// Out is the gitignored run root; each pass is <out>/pass-<n>.
	Out string `json:"out"`
	// AppMap is the rendered, drift-checked app map.
	AppMap string `json:"appMap"`
	// Spec is the written design spec reviewers judge against.
	Spec string `json:"spec,omitempty"`
	// Runner runs the repo's Playwright ("pnpm exec playwright test").
	Runner string `json:"runner"`
	// TSRunner runs a TypeScript file ("pnpm exec tsx", "bun"); see TSRunnerOrDefault.
	TSRunner string `json:"tsRunner,omitempty"`
	// Areas are the capture and publish order.
	Areas     []string            `json:"areas"`
	Apps      map[string]App      `json:"apps"`
	Viewports map[string]Viewport `json:"viewports,omitempty"`
	Lint      Lint                `json:"lint,omitempty"`
	Vitrinka  Vitrinka            `json:"vitrinka"`
	Publish   Publish             `json:"publish,omitempty"`
	// Source is the application source as git pathspecs: a change there
	// stales a pass (state's drift.app). Empty is DefaultSource.
	Source []string `json:"source,omitempty"`
	// Primitives are directory prefixes holding shared primitives: the fix
	// lanes' default, and a change under one makes a verify a full reshoot.
	Primitives []string `json:"primitives,omitempty"`
	Review     Review   `json:"review,omitempty"`
}

// Review tunes the review stage.
type Review struct {
	// CarryTolerance is the fraction of a shot's pixels that may differ from
	// the previous pass's shot for its screen to carry that pass's review
	// (planCarry); nil is DefaultCarryTolerance, 0 asks for identical pixels.
	CarryTolerance *float64 `json:"carryTolerance,omitempty"`
}

// CarryToleranceOrDefault is Review.CarryTolerance, else DefaultCarryTolerance.
func (c Config) CarryToleranceOrDefault() float64 {
	if c.Review.CarryTolerance != nil {
		return *c.Review.CarryTolerance
	}
	return DefaultCarryTolerance
}

// App is one app of the repo the loop shoots.
type App struct {
	BaseURL string `json:"baseUrl"`
	// Env names an env var that overrides BaseURL wherever it is set; the
	// capture machine's value wins last.
	Env       string   `json:"env,omitempty"`
	Viewports []string `json:"viewports"`
	Themes    []string `json:"themes"`
}

// baseURLHere is the base URL a capture started on this machine shoots: the
// app's env var when it is set here, else baseUrl (harness/run.ts baseUrlOf).
// from names the source: "$<ENV>" or "baseUrl".
func (a App) baseURLHere() (base, from string) {
	if a.Env != "" {
		if v := os.Getenv(a.Env); v != "" {
			return v, "$" + a.Env
		}
	}
	return a.BaseURL, "baseUrl"
}

// Viewport is a capture size; Mobile means a coarse pointer.
type Viewport struct {
	Width  int     `json:"width"`
	Height int     `json:"height"`
	Mobile bool    `json:"mobile,omitempty"`
	Insets *Insets `json:"insets,omitempty"`
}

// Insets are the emulated safe-area insets in CSS px.
type Insets struct {
	Top    int `json:"top"`
	Right  int `json:"right"`
	Bottom int `json:"bottom"`
	Left   int `json:"left"`
}

// Lint tunes the in-page lint (harness/lint.ts).
type Lint struct {
	Grid        int       `json:"grid,omitempty"`
	TouchTarget int       `json:"touchTarget,omitempty"`
	Off         []string  `json:"off,omitempty"`
	Ramp        []float64 `json:"ramp,omitempty"`
	// Allow maps a defect rule id to CSS selectors: a hit on an element that
	// matches one, or sits inside one, is counted as info, not as a defect
	// (a spec that allows half steps inside primitive recipes only).
	Allow map[string][]string `json:"allow,omitempty"`
}

// Vitrinka is where passes are published.
type Vitrinka struct {
	Project     string `json:"project"`
	BoardPrefix string `json:"boardPrefix"`
}

// Publish says where `publish --follow` fetches a pass from.
type Publish struct {
	// MaxFiles and MaxBytes are deprecated and ignored (a pass publishes one
	// set per area); still accepted so an older config decodes, with a
	// CONFIG_DEPRECATED warning.
	MaxFiles int   `json:"maxFiles,omitempty"`
	MaxBytes int64 `json:"maxBytes,omitempty"`
	// From is the rsync source of the repo on the capture box
	// (devops:ws/<workspace>/<app-dir>); follow appends /<out>/pass-<n>/.
	From string `json:"from,omitempty"`
}

// Defaults and hard limits.
const (
	DefaultGrid        = 4
	DefaultTouchTarget = 44
	// DefaultCarryTolerance is uiLoop.review.carryTolerance's default.
	DefaultCarryTolerance = 0.001
	// MaxSetFiles mirrors vitrinka's ingest.MaxSetFiles: the files one set
	// holds on the per-file door, its manifest included.
	MaxSetFiles = 20_000
)

// LintRules mirrors LINT_RULES in harness/lint.ts (a test keeps them equal).
var LintRules = []string{
	"h-scroll", "h-scroller", "text-clipped", "text-spill", "truncated", "grid", "type-ramp",
	"touch-target", "safe-area", "repeated-text", "contrast", "glass-on-content", "nested-surface", "glass-blur",
}

// LintInfoRules are the LintRules whose hits are listed for judgement, never
// counted as defects (LINT_RULES kind 'info'; the same test keeps them equal).
var LintInfoRules = []string{"h-scroller", "truncated", "repeated-text"}

var (
	kebab  = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	envVar = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
)

// WithDefaults fills every optional knob.
func (c Config) WithDefaults() Config {
	if c.Lint.Grid == 0 {
		c.Lint.Grid = DefaultGrid
	}
	if c.Lint.TouchTarget == 0 {
		c.Lint.TouchTarget = DefaultTouchTarget
	}
	c.TSRunner = c.TSRunnerOrDefault()
	return c
}

// TSRunnerOrDefault: the configured runner, else bun in a bun repo, else npx tsx.
func (c Config) TSRunnerOrDefault() string {
	if c.TSRunner != "" {
		return c.TSRunner
	}
	if strings.HasPrefix(strings.TrimSpace(c.Runner), "bun") {
		return "bun"
	}
	return "npx --yes tsx"
}

// SourceOrDefault is Source, else the whole repo (from its top, so a config
// root below it still sees the library beside it) minus what never changes
// a shot: the run root, .vitrinka, the rig, the spec and the app map (those
// relative to Root), every Markdown file and every .claude/.
func (c Config) SourceOrDefault() []string {
	if len(c.Source) > 0 {
		return c.Source
	}
	out := []string{":(top)"}
	for _, p := range []string{c.Out, ".vitrinka", c.Dir, c.Spec, c.AppMap} {
		if p != "" {
			out = append(out, ":(exclude,literal)"+strings.TrimRight(p, "/"))
		}
	}
	return append(out, ":(top,exclude,glob)**/*.md", ":(top,exclude,glob)**/.claude/**")
}

// ResolvedViewports is the built-in table with the config's additions and overrides.
func (c Config) ResolvedViewports() map[string]Viewport {
	out := map[string]Viewport{}
	for id, v := range BuiltinViewports {
		out[id] = v
	}
	for id, v := range c.Viewports {
		out[id] = v
	}
	return out
}

// AppNames in sorted order.
func (c Config) AppNames() []string {
	names := make([]string, 0, len(c.Apps))
	for n := range c.Apps {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ThemeOrder: every theme an app shoots, in config order (apps sorted), then
// light and dark.
func (c Config) ThemeOrder() []string {
	var out []string
	for _, name := range c.AppNames() {
		for _, th := range c.Apps[name].Themes {
			if !slices.Contains(out, th) {
				out = append(out, th)
			}
		}
	}
	for _, th := range []string{"light", "dark"} {
		if !slices.Contains(out, th) {
			out = append(out, th)
		}
	}
	return out
}

// ViewportOrder: every viewport an app shoots, in config order (apps sorted), then the rest.
func (c Config) ViewportOrder() []string {
	var out []string
	for _, name := range c.AppNames() {
		for _, v := range c.Apps[name].Viewports {
			if !slices.Contains(out, v) {
				out = append(out, v)
			}
		}
	}
	return out
}

func relPath(field, p string, required bool) string {
	if p == "" {
		if required {
			return field + " is required"
		}
		return ""
	}
	if path.IsAbs(p) || strings.HasPrefix(p, "~") {
		return fmt.Sprintf("%s %q must be repo-relative", field, p)
	}
	if clean := path.Clean(p); clean == ".." || strings.HasPrefix(clean, "../") || clean == "." {
		return fmt.Sprintf("%s %q must stay inside the repo and name a path", field, p)
	}
	return ""
}

// Validate reports every problem at once.
func (c Config) Validate() []string {
	var problems []string
	add := func(s string) {
		if s != "" {
			problems = append(problems, s)
		}
	}
	add(relPath("dir", c.Dir, true))
	add(relPath("out", c.Out, true))
	add(relPath("appMap", c.AppMap, true))
	add(relPath("spec", c.Spec, false))
	if strings.TrimSpace(c.Runner) == "" {
		add("runner is required (how the repo runs Playwright, e.g. \"pnpm exec playwright test\")")
	}
	if len(c.Areas) == 0 {
		add("areas must list at least one area")
	}
	seen := map[string]bool{}
	for _, a := range c.Areas {
		if !kebab.MatchString(a) {
			add(fmt.Sprintf("area %q is not kebab-case", a))
		}
		if seen[a] {
			add(fmt.Sprintf("area %q is listed twice", a))
		}
		seen[a] = true
	}
	for id, v := range c.Viewports {
		if !kebab.MatchString(id) {
			add(fmt.Sprintf("viewport id %q is not kebab-case", id))
		}
		if v.Width <= 0 || v.Height <= 0 {
			add(fmt.Sprintf("viewports.%s needs a positive width and height", id))
		}
		if in := v.Insets; in != nil && (in.Top < 0 || in.Right < 0 || in.Bottom < 0 || in.Left < 0) {
			add(fmt.Sprintf("viewports.%s insets must not be negative", id))
		}
	}
	known := c.ResolvedViewports()
	if len(c.Apps) == 0 {
		add("apps must name at least one app")
	}
	for _, name := range c.AppNames() {
		app := c.Apps[name]
		if !kebab.MatchString(name) {
			add(fmt.Sprintf("app name %q is not kebab-case", name))
		}
		if u, err := url.Parse(app.BaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			add(fmt.Sprintf("apps.%s.baseUrl %q is not an http(s) URL", name, app.BaseURL))
		}
		if app.Env != "" && !envVar.MatchString(app.Env) {
			add(fmt.Sprintf("apps.%s.env %q is not an env var name", name, app.Env))
		}
		if len(app.Viewports) == 0 {
			add(fmt.Sprintf("apps.%s.viewports must list at least one viewport", name))
		}
		for _, v := range app.Viewports {
			if _, ok := known[v]; !ok {
				add(fmt.Sprintf("apps.%s.viewports: unknown viewport %q (built-ins: %s)", name, v, strings.Join(BuiltinViewportIDs(), ", ")))
			}
		}
		if len(app.Themes) == 0 {
			add(fmt.Sprintf("apps.%s.themes must list light and/or dark", name))
		}
		for _, t := range app.Themes {
			if t != "light" && t != "dark" {
				add(fmt.Sprintf("apps.%s.themes: %q is not light or dark", name, t))
			}
		}
	}
	if c.Lint.Grid < 0 || c.Lint.TouchTarget < 0 {
		add("lint.grid and lint.touchTarget must be positive")
	}
	for _, r := range c.Lint.Off {
		if !slices.Contains(LintRules, r) {
			add(fmt.Sprintf("lint.off: unknown rule %q (rules: %s)", r, strings.Join(LintRules, ", ")))
		}
	}
	for _, r := range slices.Sorted(maps.Keys(c.Lint.Allow)) {
		switch {
		case !slices.Contains(LintRules, r):
			add(fmt.Sprintf("lint.allow: unknown rule %q (rules: %s)", r, strings.Join(LintRules, ", ")))
		case slices.Contains(LintInfoRules, r):
			add(fmt.Sprintf("lint.allow: %q is informational already; only defect rules take an allowlist", r))
		case len(c.Lint.Allow[r]) == 0:
			add(fmt.Sprintf("lint.allow.%s must list at least one selector", r))
		}
		for _, sel := range c.Lint.Allow[r] {
			if strings.TrimSpace(sel) == "" {
				add(fmt.Sprintf("lint.allow.%s holds an empty selector", r))
			}
		}
	}
	for _, r := range c.Lint.Ramp {
		if r <= 0 {
			add("lint.ramp holds font sizes in px, all positive")
			break
		}
	}
	for _, s := range c.Source {
		if strings.TrimSpace(s) == "" {
			add("source holds an empty pathspec")
		}
	}
	for _, p := range c.Primitives {
		if strings.TrimSpace(p) == "" {
			add("primitives holds an empty prefix")
		}
		add(relPath("primitives", p, false))
	}
	if tol := c.CarryToleranceOrDefault(); tol < 0 || tol >= 1 {
		add(fmt.Sprintf("review.carryTolerance %v must be a fraction of the pixels, at least 0 and below 1", tol))
	}
	if c.Vitrinka.Project == "" {
		add("vitrinka.project is required")
	}
	if !kebab.MatchString(c.Vitrinka.BoardPrefix) {
		add(fmt.Sprintf("vitrinka.boardPrefix %q must be kebab-case", c.Vitrinka.BoardPrefix))
	} else if len(c.Vitrinka.BoardPrefix) > 24 {
		add("vitrinka.boardPrefix must be at most 24 characters (set keys cap at 64)")
	}
	return problems
}

// Deprecations warn about keys still accepted but ignored.
func (c Config) Deprecations() []runxDiagnostic {
	if c.Publish.MaxFiles == 0 && c.Publish.MaxBytes == 0 {
		return nil
	}
	return []runxDiagnostic{warn(DiagConfigDeprecated,
		"publish.maxFiles and publish.maxBytes are ignored: a pass publishes one vitrinka set per area",
		"delete them from the uiLoop section of vybava.config.ts")}
}

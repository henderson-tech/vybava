// Package reconcile is the pull-based GitOps engine for the infra boxes: a
// cron tick pulls origin/main into a clone and converges the mapped files onto
// the box. It is a port of the bash `scripts/infra-reconcile/reconcile.sh`
// engine (webulinka-infra is the reference) driven by a per-box YAML manifest
// that carries the bash `map-paths.sh` semantics verbatim.
package reconcile

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Hook names a mapping's post-converge action.
type Hook string

const (
	HookNone    Hook = "none"
	HookNginx   Hook = "nginx"
	HookCompose Hook = "compose"
)

// Manifest is the per-box `reconcile.yaml` at the clone root.
type Manifest struct {
	SchemaVersion int    `yaml:"schema_version" json:"schema_version"`
	Repo          string `yaml:"repo" json:"repo"`
	HostLabel     string `yaml:"host_label" json:"host_label"`
	// Clone is the box-side checkout; defaults to the manifest's directory.
	Clone    string `yaml:"clone,omitempty" json:"clone"`
	ModeFile string `yaml:"mode_file,omitempty" json:"mode_file"`
	StateDir string `yaml:"state_dir,omitempty" json:"state_dir"`
	LockFile string `yaml:"lock_file,omitempty" json:"lock_file"`
	// AppsRoot is the compose containment root (`/opt/apps`).
	AppsRoot string `yaml:"apps_root,omitempty" json:"apps_root"`
	// MetricsFile is the node-exporter textfile written per tick (optional).
	MetricsFile string `yaml:"metrics_file,omitempty" json:"metrics_file,omitempty"`
	// VybavaVersion pins the release the box is expected to run (optional).
	VybavaVersion string `yaml:"vybava_version,omitempty" json:"vybava_version,omitempty"`

	// Skip globs are evaluated before every mapping (bash `case` semantics:
	// `*` matches across `/`). Ordered exceptions to a skip go into mappings.
	Skip     []string  `yaml:"skip,omitempty" json:"skip,omitempty"`
	Mappings []Mapping `yaml:"mappings" json:"mappings"`
	// AutoRollApps opt into `docker compose up -d` after a compose converge.
	AutoRollApps []string `yaml:"auto_roll_apps,omitempty" json:"auto_roll_apps,omitempty"`
	// RollNotes claim converged files `docker compose up -d` does not apply
	// (bind-mounted DB config) and name their exact step instead of the
	// generic ROLL MANUALLY — rollnotes.go.
	RollNotes []RollNote `yaml:"roll_notes,omitempty" json:"roll_notes,omitempty"`

	Hooks  Hooks       `yaml:"hooks,omitempty" json:"hooks"`
	Alerts []Alert     `yaml:"alerts,omitempty" json:"alerts,omitempty"`
	Serve  ServeConfig `yaml:"serve,omitempty" json:"serve"`
}

// Mapping is one ordered `case` arm: first match wins. A `skip: true` arm
// excludes the path; otherwise `dest` receives the file. `{rest}` is the path
// with `strip` removed, `{app}` its first component, `{path}` the full path.
type Mapping struct {
	Match []string `yaml:"match" json:"match"`
	Skip  bool     `yaml:"skip,omitempty" json:"skip,omitempty"`
	Strip string   `yaml:"strip,omitempty" json:"strip,omitempty"`
	Dest  string   `yaml:"dest,omitempty" json:"dest,omitempty"`
	Hook  Hook     `yaml:"hook,omitempty" json:"hook,omitempty"`
	// App overrides the compose app name derived from `{app}`.
	App string `yaml:"app,omitempty" json:"app,omitempty"`
	// RequireLiveDir refuses to materialize `<apps_root>/<app>` when absent
	// and contains every write inside it. Defaults to true for compose.
	RequireLiveDir *bool `yaml:"require_live_dir,omitempty" json:"require_live_dir,omitempty"`
	// Owner is the expected owner of the destination tree; a permission
	// failure is classified against it instead of merely logged.
	Owner   string `yaml:"owner,omitempty" json:"owner,omitempty"`
	Comment string `yaml:"comment,omitempty" json:"comment,omitempty"`
}

// Hooks are the per-box commands behind the nginx hook. Each is an argument
// array run in `workdir`; a fake binary on PATH is enough to test them.
type Hooks struct {
	Nginx NginxHook `yaml:"nginx,omitempty" json:"nginx"`
	// Compose is the auto-roll command run inside `<apps_root>/<app>`;
	// defaults to `docker compose up -d`.
	Compose []string `yaml:"compose,omitempty" json:"compose,omitempty"`
}

type NginxHook struct {
	Workdir string   `yaml:"workdir,omitempty" json:"workdir,omitempty"`
	Test    []string `yaml:"test,omitempty" json:"test,omitempty"`
	Reload  []string `yaml:"reload,omitempty" json:"reload,omitempty"`
	// CertsPresent probes a TLS vhost's certificates before it may move: the
	// vhost's ssl_certificate / ssl_certificate_key paths are appended as
	// arguments, exit 0 = every one exists where nginx will look (inside the
	// proxy container). Absent = no certificate hold — certhold.go.
	CertsPresent []string `yaml:"certs_present,omitempty" json:"certs_present,omitempty"`
}

// Alert is one digest channel; both keep their own dedup marker.
type Alert struct {
	Type string `yaml:"type" json:"type"` // telegram | eve-monitor
	// Telegram: the shell library exposing notify_telegram, and its channel.
	Lib     string `yaml:"lib,omitempty" json:"lib,omitempty"`
	Channel string `yaml:"channel,omitempty" json:"channel,omitempty"`
	// Eve monitor: the KEY=VALUE config file (EVE_MONITOR_URL/_TOKEN).
	Config string `yaml:"config,omitempty" json:"config,omitempty"`
}

type ServeConfig struct {
	// Listen is the WireGuard address the read-only status page binds to.
	Listen string `yaml:"listen,omitempty" json:"listen,omitempty"`
	// Hosts lists the boxes a hub polls: name → status URL.
	Hosts []HubHost `yaml:"hosts,omitempty" json:"hosts,omitempty"`
}

type HubHost struct {
	Name string `yaml:"name" json:"name"`
	URL  string `yaml:"url" json:"url"`
}

// Load reads, defaults and validates a manifest. Relative paths resolve
// against the manifest's directory; `~/` expands to the caller's home.
func Load(manifestPath string) (Manifest, error) {
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return Manifest{}, fmt.Errorf("parse %s: %w", manifestPath, err)
	}
	abs, err := filepath.Abs(manifestPath)
	if err != nil {
		return Manifest{}, err
	}
	if err := m.Finalize(filepath.Dir(abs)); err != nil {
		return Manifest{}, fmt.Errorf("validate %s: %w", manifestPath, err)
	}
	return m, nil
}

// Parse validates a manifest from memory without touching path defaults that
// depend on a location (used by the contract tests over the fixture maps).
func Parse(raw []byte) (Manifest, error) {
	var m Manifest
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return Manifest{}, err
	}
	if err := m.Finalize(""); err != nil {
		return Manifest{}, err
	}
	return m, nil
}

// Finalize applies defaults and validates. base is the manifest directory.
func (m *Manifest) Finalize(base string) error {
	if m.SchemaVersion != 1 {
		return fmt.Errorf("unsupported schema_version %d (want 1)", m.SchemaVersion)
	}
	if m.Repo == "" {
		return fmt.Errorf("repo is required")
	}
	if m.HostLabel == "" {
		return fmt.Errorf("host_label is required")
	}
	if len(m.Mappings) == 0 {
		return fmt.Errorf("mappings must not be empty")
	}
	home, _ := os.UserHomeDir()
	resolve := func(p, fallback string) (string, error) {
		if p == "" {
			p = fallback
		}
		if p == "" {
			return "", nil
		}
		if p == "~" || strings.HasPrefix(p, "~/") {
			if home == "" {
				return "", fmt.Errorf("cannot expand %q: no home directory", p)
			}
			p = filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
		if !filepath.IsAbs(p) && base != "" {
			p = filepath.Join(base, p)
		}
		return filepath.Clean(p), nil
	}
	var err error
	if m.Clone, err = resolve(m.Clone, base); err != nil {
		return err
	}
	if m.ModeFile, err = resolve(m.ModeFile, "~/.config/infra-reconcile/mode"); err != nil {
		return err
	}
	if m.StateDir, err = resolve(m.StateDir, "~/.local/state/infra-reconcile"); err != nil {
		return err
	}
	if m.LockFile, err = resolve(m.LockFile, "/tmp/infra-reconcile.lock"); err != nil {
		return err
	}
	if m.AppsRoot, err = resolve(m.AppsRoot, "/opt/apps"); err != nil {
		return err
	}
	if m.MetricsFile, err = resolve(m.MetricsFile, ""); err != nil {
		return err
	}
	if m.Hooks.Nginx.Workdir, err = resolve(m.Hooks.Nginx.Workdir, ""); err != nil {
		return err
	}
	if len(m.Hooks.Compose) == 0 {
		m.Hooks.Compose = []string{"docker", "compose", "up", "-d"}
	}
	for i := range m.Alerts {
		a := &m.Alerts[i]
		switch a.Type {
		case "telegram":
			if a.Lib, err = resolve(a.Lib, "/opt/scripts/lib/telegram-notify.sh"); err != nil {
				return err
			}
			if a.Channel == "" {
				a.Channel = "lovinka_monitoring"
			}
		case "eve-monitor":
			if a.Config, err = resolve(a.Config, "~/.config/infra-reconcile/eve-webhook"); err != nil {
				return err
			}
		default:
			return fmt.Errorf("alerts[%d]: unknown type %q (telegram | eve-monitor)", i, a.Type)
		}
	}
	for i, g := range m.Skip {
		if g == "" {
			return fmt.Errorf("skip[%d]: empty glob", i)
		}
	}
	for i := range m.Mappings {
		mp := &m.Mappings[i]
		if len(mp.Match) == 0 {
			return fmt.Errorf("mappings[%d]: match is required", i)
		}
		if mp.Skip {
			if mp.Dest != "" || mp.Hook != "" {
				return fmt.Errorf("mappings[%d]: a skip arm carries no dest or hook", i)
			}
			continue
		}
		if mp.Dest == "" || !filepath.IsAbs(mp.Dest) {
			return fmt.Errorf("mappings[%d]: dest must be an absolute path", i)
		}
		if mp.Hook == "" {
			mp.Hook = HookNone
		}
		switch mp.Hook {
		case HookNone, HookNginx, HookCompose:
		default:
			return fmt.Errorf("mappings[%d]: unknown hook %q (nginx | compose | none)", i, mp.Hook)
		}
		if mp.RequireLiveDir == nil {
			v := mp.Hook == HookCompose
			mp.RequireLiveDir = &v
		}
		if *mp.RequireLiveDir && mp.Hook != HookCompose {
			return fmt.Errorf("mappings[%d]: require_live_dir needs hook: compose", i)
		}
	}
	for i, r := range m.RollNotes {
		if err := r.validate(); err != nil {
			return fmt.Errorf("roll_notes[%d]: %w", i, err)
		}
	}
	return nil
}

// Target is a resolved mapping for one repo path.
type Target struct {
	Dest string
	Hook Hook
	// App is the compose app (hook compose only).
	App string
	// RequireLiveDir contains the write inside <apps_root>/<app>.
	RequireLiveDir bool
	Owner          string
}

// MapPath resolves a repo-relative path; ok=false means skipped.
func (m Manifest) MapPath(rp string) (Target, bool) {
	for _, g := range m.Skip {
		if globMatch(g, rp) {
			return Target{}, false
		}
	}
	for _, mp := range m.Mappings {
		matched := false
		for _, g := range mp.Match {
			if globMatch(g, rp) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}
		if mp.Skip {
			return Target{}, false
		}
		rest := rp
		if mp.Strip != "" {
			rest = strings.TrimPrefix(rp, mp.Strip)
		}
		app := mp.App
		if app == "" {
			app, _, _ = strings.Cut(rest, "/")
		}
		dest := strings.NewReplacer("{rest}", rest, "{app}", app, "{path}", rp).Replace(mp.Dest)
		t := Target{Dest: path.Clean(dest), Hook: mp.Hook, Owner: mp.Owner}
		if mp.Hook == HookCompose {
			t.App = app
		}
		if mp.RequireLiveDir != nil {
			t.RequireLiveDir = *mp.RequireLiveDir
		}
		return t, true
	}
	return Target{}, false
}

// globMatch mirrors bash `case` patterns: `*` and `?` match `/` too.
func globMatch(pattern, s string) bool {
	re, err := globRegexp(pattern)
	if err != nil {
		return false
	}
	return re.MatchString(s)
}

func globRegexp(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; c {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		case '[':
			end := strings.IndexByte(pattern[i+1:], ']')
			if end < 0 {
				b.WriteString(regexp.QuoteMeta("["))
				continue
			}
			class := pattern[i+1 : i+1+end]
			if strings.HasPrefix(class, "!") {
				class = "^" + class[1:]
			}
			b.WriteString("[" + class + "]")
			i += end + 1
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

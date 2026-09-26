package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/henderson-tech/vybava/internal/vpn"
	"github.com/spf13/cobra"
)

func (rt *runtime) vpnApplet() *cobra.Command {
	c := rt.vpnCommand()
	c.SilenceUsage = true
	c.SilenceErrors = true
	c.SetOut(rt.stdout)
	c.SetErr(rt.stderr)
	c.PersistentFlags().BoolVar(&rt.json, "json", false, "emit stable JSON output")
	return c
}

func (rt *runtime) vpnCommand() *cobra.Command {
	c := &cobra.Command{
		Use:   "vpn",
		Short: "Persistent macOS WireGuard tunnels from Onyx profiles, with an honest status",
		Long: `Runs named WireGuard tunnels outside WireGuard.app as LaunchDaemons that start
at boot and restart when the interface dies. The profile comes from Onyx at
install time and rests only in a root-only file. status reports what the
kernel routes, beside what WireGuard.app and launchd claim: an app profile
reading Disconnected while wg-quick carries the tunnel is normal.`,
		Example: `  vybava vpn add lovinka-admin --ref 'onyx://WireGuard/…/Configuration' --probe 10.8.1.1:443 --dns 10.8.1.1
  vybava vpn install lovinka-admin --dry-run     # the sudo steps, nothing run
  vybava vpn install lovinka-admin               # Onyx approval, then sudo
  vybava vpn status --json`,
	}
	session := func(cmd *cobra.Command) *runx.Session {
		return &runx.Session{Tool: "vpn", JSON: rt.json, Verb: "vpn " + cmd.Name(), Stdout: rt.stdout, Stderr: rt.stderr}
	}
	// finish emits the one envelope; in text mode text() replaces the data
	// dump and the envelope adds only diagnostics and next commands.
	finish := func(s *runx.Session, data any, text func(), diags []runx.Diagnostic, next []string) error {
		env := runx.Envelope{OK: true, Verb: s.Verb, Data: data, Diagnostics: diags, Next: next}
		for _, d := range diags {
			env.OK = env.OK && d.Severity != "error"
		}
		if !rt.json {
			env.Data = nil
			if text != nil {
				text()
			}
		}
		if err := s.Emit(env); err != nil {
			return err
		}
		if !env.OK {
			return runx.ExitError{Code: 2}
		}
		return nil
	}
	fail := func(s *runx.Session, code string, err error, fix string) error {
		next := []string{}
		if fix != "" {
			next = append(next, fix)
		}
		return finish(s, nil, nil, []runx.Diagnostic{{Code: code, Severity: "error", Detail: err.Error(), Fix: fix}}, next)
	}
	addFix := func(name string) string {
		return "vybava vpn add " + name + " --ref onyx://<vault>/<item>/<field> --probe <host:port> --dns <ip>"
	}
	darwin := func(s *runx.Session) error {
		if goruntime.GOOS != "darwin" {
			return fail(s, "VPN_UNSUPPORTED", errors.New("tunnels are managed on macOS only"), "")
		}
		return nil
	}

	var p vpn.Profile
	add := &cobra.Command{
		Use:   "add NAME",
		Short: "Register or update a tunnel: vault reference, probes, DNS server (no keys)",
		Long:  "Creates the registration, or updates only the flags given on an existing one.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, name := session(cmd), args[0]
			dir, err := vpn.Directory()
			if err != nil {
				return fail(s, runx.DiagInfraError, err, "")
			}
			current, found, err := vpn.Read(dir, name)
			if err != nil {
				return fail(s, "VPN_PROFILE", err, "")
			}
			flags := cmd.Flags()
			if flags.Changed("ref") {
				current.Ref = p.Ref
			}
			if flags.Changed("probe") {
				current.Probes = p.Probes
			}
			if flags.Changed("dns") {
				current.DNS = p.DNS
			}
			if flags.Changed("exclude-peer") {
				current.ExcludePeers = p.ExcludePeers
			}
			if err := vpn.Save(dir, name, current); err != nil {
				return fail(s, "VPN_PROFILE", err, addFix(name))
			}
			verb := map[bool]string{true: "Updated", false: "Registered"}[found]
			return finish(s, map[string]any{"name": name, "created": !found, "profile": current},
				func() { fmt.Fprintf(rt.stdout, "%s %s in %s.\n", verb, name, filepath.Join(dir, name+".json")) },
				[]runx.Diagnostic{}, []string{"vybava vpn install " + name})
		},
	}
	add.Flags().StringVar(&p.Ref, "ref", "", "Onyx reference holding the full WireGuard config")
	add.Flags().StringSliceVar(&p.Probes, "probe", nil, "TCP host:port the tunnel must reach (repeatable)")
	add.Flags().StringVar(&p.DNS, "dns", "", "DNS server the tunnel carries (ip[:port]); status asks it a question")
	add.Flags().StringSliceVar(&p.ExcludePeers, "exclude-peer", nil, "retired peer public key to omit from the vault copy")

	var dryRun bool
	plan := func(steps []vpn.Step) map[string]any {
		lines := make([]string, len(steps))
		for i, step := range steps {
			lines[i] = step.Command()
		}
		return map[string]any{"steps": steps, "commands": lines}
	}
	printPlan := func(steps []vpn.Step) {
		for _, step := range steps {
			fmt.Fprintf(rt.stdout, "# %s\n%s\n", step.Why, step.Command())
		}
	}

	install := &cobra.Command{
		Use:   "install NAME",
		Short: "Make the tunnel a persistent LaunchDaemon (Onyx approval, then sudo)",
		Long: `Pulls the profile from Onyx, writes it root-only to ` + vpn.ConfigDir + `/NAME.conf,
installs ` + vpn.DaemonDir + `/com.vybava.vpn.NAME.plist and (re)starts it. Any job
already holding the label — the transient recovery job included — is stopped
first, so re-running install is a restart with the vault's current profile.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, name, ctx := session(cmd), args[0], cmd.Context()
			if err := darwin(s); err != nil {
				return err
			}
			dir, err := vpn.Directory()
			if err != nil {
				return fail(s, runx.DiagInfraError, err, "")
			}
			profile, err := vpn.Load(dir, name)
			if err != nil {
				return fail(s, "VPN_PROFILE", err, addFix(name))
			}
			bin, err := vpn.HomebrewBin()
			if err != nil {
				return fail(s, "VPN_PREREQ", err, "brew install bash wireguard-tools wireguard-go")
			}
			m := vpn.System{}
			if m.AppState(ctx, name) == "Connected" {
				return fail(s, "VPN_DUPLICATE", fmt.Errorf("WireGuard.app has %s connected; one identity must not run twice", name), "turn "+name+" off in WireGuard.app")
			}
			svc, err := m.Service(ctx, vpn.Label(name))
			if err != nil {
				return fail(s, runx.DiagInfraError, err, "")
			}
			if dryRun {
				steps := vpn.InstallPlan(name, bin, "", svc.Loaded)
				return finish(s, plan(steps), func() { printPlan(steps) }, []runx.Diagnostic{}, []string{"vybava vpn install " + name})
			}
			exe, err := os.Executable()
			if err == nil {
				exe, err = filepath.EvalSymlinks(exe)
			}
			if err != nil {
				return fail(s, runx.DiagInfraError, err, "")
			}
			config, err := vpn.Fetch(ctx, exe, dir, name, profile)
			if err != nil {
				bind := strings.Join(vpn.ApplyArgv(exe, name, "", "")[:4], " ")
				return fail(s, "VPN_ONYX", fmt.Errorf("%w (the vault item's allowed_commands must permit `%s`)", err, bind), "vybava vpn install "+name)
			}
			fmt.Fprintf(rt.stderr, "Installing %s — sudo asks for your password once.\n", vpn.Label(name))
			if err := vpn.Run(ctx, vpn.InstallPlan(name, bin, config, svc.Loaded), rt.stderr); err != nil {
				return fail(s, "VPN_PRIVILEGED", err, "vybava vpn install "+name)
			}
			st, err := settle(ctx, m, name, profile)
			if err != nil {
				return fail(s, runx.DiagInfraError, err, "")
			}
			return rt.vpnReport(s, finish, []vpn.Status{st}, "vybava vpn status "+name)
		},
	}
	install.Flags().BoolVar(&dryRun, "dry-run", false, "print the privileged steps without fetching the profile or running sudo")

	uninstall := &cobra.Command{
		Use:   "uninstall NAME",
		Short: "Stop the tunnel and remove its LaunchDaemon, supervisor and key (sudo)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, name, ctx := session(cmd), args[0], cmd.Context()
			if err := darwin(s); err != nil {
				return err
			}
			if err := vpn.ValidateName(name); err != nil {
				return fail(s, "VPN_PROFILE", err, "")
			}
			svc, err := (vpn.System{}).Service(ctx, vpn.Label(name))
			if err != nil {
				return fail(s, runx.DiagInfraError, err, "")
			}
			steps := vpn.UninstallPlan(name, svc.Loaded)
			if dryRun {
				return finish(s, plan(steps), func() { printPlan(steps) }, []runx.Diagnostic{}, []string{"vybava vpn uninstall " + name})
			}
			if err := vpn.Run(ctx, steps, rt.stderr); err != nil {
				return fail(s, "VPN_PRIVILEGED", err, "")
			}
			return finish(s, map[string]any{"name": name, "stopped": svc.Loaded, "removed": steps[len(steps)-1].Argv[2:]},
				func() { fmt.Fprintf(rt.stdout, "Removed %s (stopped: %t).\n", vpn.Label(name), svc.Loaded) },
				[]runx.Diagnostic{}, []string{"vybava vpn status " + name})
		},
	}
	uninstall.Flags().BoolVar(&dryRun, "dry-run", false, "print the privileged steps without running sudo")

	status := &cobra.Command{
		Use:   "status [NAME...]",
		Short: "Report each tunnel: route truth, wg-quick interface, daemon, app state, DNS, probes",
		Long:  "Exits 2 when a tunnel is down or its DNS is silent. No NAME reports every registered tunnel.",
		RunE: func(cmd *cobra.Command, args []string) error {
			s, ctx := session(cmd), cmd.Context()
			if err := darwin(s); err != nil {
				return err
			}
			dir, err := vpn.Directory()
			if err != nil {
				return fail(s, runx.DiagInfraError, err, "")
			}
			names := args
			if len(names) == 0 {
				if names, err = vpn.Names(dir); err != nil {
					return fail(s, runx.DiagInfraError, err, "")
				}
				if len(names) == 0 {
					return fail(s, "VPN_PROFILE", fmt.Errorf("no tunnel is registered in %s", dir), addFix("<name>"))
				}
			}
			statuses := []vpn.Status{}
			for _, name := range names {
				profile, err := vpn.Load(dir, name)
				if err != nil {
					return fail(s, "VPN_PROFILE", err, addFix(name))
				}
				st, err := vpn.Inspect(ctx, vpn.System{}, name, profile)
				if err != nil {
					return fail(s, runx.DiagInfraError, err, "")
				}
				statuses = append(statuses, st)
			}
			return rt.vpnReport(s, finish, statuses, "")
		},
	}

	apply := &cobra.Command{
		Use: "_apply NAME CONFIG_DIR FIFO", Hidden: true, Args: cobra.ExactArgs(3),
		RunE: func(_ *cobra.Command, args []string) error {
			return vpn.Deliver(args[1], args[0], args[2])
		},
	}
	c.AddCommand(add, install, uninstall, status, apply)
	return c
}

// settle waits for a freshly bootstrapped daemon to carry the tunnel.
func settle(ctx context.Context, m vpn.Machine, name string, p vpn.Profile) (vpn.Status, error) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		st, err := vpn.Inspect(ctx, m, name, p)
		if err != nil || st.State == "up" || time.Now().After(deadline) {
			return st, err
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// vpnReport renders statuses: one block per tunnel in text, the list as
// data in JSON; every diagnostic's fix becomes a next command.
func (rt *runtime) vpnReport(s *runx.Session, finish func(*runx.Session, any, func(), []runx.Diagnostic, []string) error, statuses []vpn.Status, next string) error {
	diags := []runx.Diagnostic{}
	nexts := []string{}
	seen := map[string]bool{}
	for _, st := range statuses {
		for _, d := range vpn.Diagnose(st) {
			diags = append(diags, d)
			if d.Fix != "" && !seen[d.Fix] {
				seen[d.Fix] = true
				nexts = append(nexts, d.Fix)
			}
		}
	}
	if next != "" && len(nexts) == 0 {
		nexts = append(nexts, next)
	}
	text := func() {
		for _, st := range statuses {
			fmt.Fprintf(rt.stdout, "%s  %s\n", st.Name, st.Summary)
			fmt.Fprintf(rt.stdout, "  app     %s (WireGuard.app / scutil --nc)\n", st.App)
			fmt.Fprintf(rt.stdout, "  daemon  %s\n", daemonText(st))
			if st.DNS != nil {
				fmt.Fprintf(rt.stdout, "  dns     %s\n", checkText(*st.DNS, "answers"))
			}
			probes := make([]string, len(st.Probes))
			for i, c := range st.Probes {
				probes[i] = checkText(c, "ok")
			}
			fmt.Fprintf(rt.stdout, "  probes  %s\n", strings.Join(probes, " · "))
			fmt.Fprintf(rt.stdout, "  log     %s\n", st.Log)
		}
	}
	return finish(s, statuses, text, diags, nexts)
}

func daemonText(st vpn.Status) string {
	d := st.Daemon
	running := ""
	if d.Running {
		running = fmt.Sprintf(", running (pid %d)", d.PID)
	}
	switch d.Kind {
	case "persistent":
		return "persistent LaunchDaemon" + running + " — " + vpn.PlistPath(st.Name)
	case "transient":
		return "transient launchctl submit job" + running + " — gone at reboot"
	case "other":
		return "a launchd job not installed by vybava" + running
	}
	if d.Installed {
		return "installed, not loaded — " + vpn.PlistPath(st.Name)
	}
	return "none"
}

func checkText(c vpn.Check, ok string) string {
	if c.OK {
		return c.Target + " " + ok
	}
	return c.Target + " FAILED (" + c.Error + ")"
}

package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	assets "github.com/henderson-tech/vybava"
	"github.com/henderson-tech/vybava/internal/catalog"
	"github.com/henderson-tech/vybava/internal/codexsync"
	"github.com/henderson-tech/vybava/internal/doctor"
	"github.com/henderson-tech/vybava/internal/fontfreeze"
	"github.com/henderson-tech/vybava/internal/ingressgen"
	"github.com/henderson-tech/vybava/internal/installer"
	"github.com/henderson-tech/vybava/internal/memorylint"
	"github.com/henderson-tech/vybava/internal/perfrig"
	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/henderson-tech/vybava/internal/state"
	"github.com/henderson-tech/vybava/internal/ui"
	"github.com/spf13/cobra"
)

var ErrFindings = errors.New("lint findings")

// ErrHookBlocked exits 2, which is how a Claude Code / Codex PreToolUse hook
// refuses the write. The hook prints its own reason, so ErrorText stays quiet.
var ErrHookBlocked = errors.New("hook blocked the write")

type App struct {
	Version string
	Stdout  io.Writer
	Stderr  io.Writer
	Stdin   io.Reader
}

type runtime struct {
	catalog   catalog.Catalog
	installer installer.Installer
	json      bool
	version   string
	stdout    io.Writer
	stderr    io.Writer
	stdin     io.Reader
}

func (a App) Command(invokedAs string) (*cobra.Command, error) {
	if a.Stdout == nil {
		a.Stdout = os.Stdout
	}
	if a.Stderr == nil {
		a.Stderr = os.Stderr
	}
	if a.Stdin == nil {
		a.Stdin = os.Stdin
	}
	c, err := catalog.Load(assets.FS)
	if err != nil {
		return nil, err
	}
	store, err := state.DefaultStore()
	if err != nil {
		return nil, err
	}
	rt := &runtime{
		catalog: c, stdout: a.Stdout, stderr: a.Stderr, stdin: a.Stdin, version: a.Version,
		installer: installer.Installer{Payload: assets.FS, Store: store},
	}
	if filepath.Base(invokedAs) == "memorylint" {
		return rt.memorylintApplet(), nil
	}
	if filepath.Base(invokedAs) == "fontfreeze" {
		return rt.fontfreezeApplet(), nil
	}
	if filepath.Base(invokedAs) == "perfrig" {
		return rt.perfrigCommand("perfrig"), nil
	}
	if filepath.Base(invokedAs) == "shrt" {
		return rt.shrtApplet(), nil
	}
	if filepath.Base(invokedAs) == "press" {
		return rt.pressApplet(), nil
	}
	if filepath.Base(invokedAs) == "ingressgen" {
		return rt.ingressgenApplet(), nil
	}
	if filepath.Base(invokedAs) == "hotfix" {
		return rt.hotfixApplet(), nil
	}
	if filepath.Base(invokedAs) == "codexsync" {
		return rt.codexsyncApplet(), nil
	}
	if filepath.Base(invokedAs) == "codexusage" {
		return rt.codexusageApplet(), nil
	}
	if filepath.Base(invokedAs) == "worktime" {
		return rt.worktimeApplet(), nil
	}
	if filepath.Base(invokedAs) == "reconcile" {
		return rt.reconcileApplet(), nil
	}
	if filepath.Base(invokedAs) == "reclaim" {
		return rt.reclaimApplet(), nil
	}
	if filepath.Base(invokedAs) == "macwatch" {
		return rt.macwatchApplet(), nil
	}
	if filepath.Base(invokedAs) == "plugin-gc" {
		return rt.pluginGCApplet(), nil
	}
	if filepath.Base(invokedAs) == "handoffs" {
		return rt.handoffsApplet(), nil
	}
	if filepath.Base(invokedAs) == "operator" {
		return rt.operatorApplet(), nil
	}
	if filepath.Base(invokedAs) == "plaud" {
		return rt.plaudApplet(), nil
	}
	if filepath.Base(invokedAs) == "envbridge" {
		return rt.envbridgeApplet(), nil
	}
	if filepath.Base(invokedAs) == "claude-guards" {
		return rt.claudeGuardsApplet(), nil
	}
	if filepath.Base(invokedAs) == "lok" {
		return rt.lokApplet(), nil
	}
	if filepath.Base(invokedAs) == "memo" {
		return rt.memoApplet(), nil
	}
	if filepath.Base(invokedAs) == "posta" {
		return rt.postaApplet(), nil
	}
	if filepath.Base(invokedAs) == "repolicy" {
		return rt.repolicyApplet(), nil
	}
	if filepath.Base(invokedAs) == "menubar-doctor" {
		return rt.menubarApplet(), nil
	}

	root := &cobra.Command{
		Use:           "vybava",
		Short:         "Install and maintain FixIt Technologies engineering tools and AI skills",
		Version:       a.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.SetOut(a.Stdout)
	root.SetErr(a.Stderr)
	root.PersistentFlags().BoolVar(&rt.json, "json", false, "emit stable JSON output")
	root.AddCommand(
		rt.catalogCommand(),
		rt.installCommand(),
		rt.uninstallCommand(),
		rt.updateCommand(),
		rt.doctorCommand(),
		rt.memoryCommand(),
		rt.fontfreezeCommand("fontfreeze [fonts.yaml]"),
		rt.perfrigCommand("perfrig"),
		rt.shrtCommand("shrt [url...]"),
		rt.pressCommand("press"),
		rt.ingressgenCommand("ingressgen"),
		rt.hotfixCommand("hotfix"),
		rt.codexsyncCommand("codexsync"),
		rt.codexusageCommand("codexusage"),
		rt.worktimeCommand("worktime"),
		rt.reconcileCommand("reconcile"),
		rt.menubarCommand("menubar-doctor"),
		rt.reclaimCommand("reclaim"),
		rt.macwatchCommand("macwatch"),
		rt.pluginGCCommand("plugin-gc"),
		rt.handoffsCommand("handoffs"),
		rt.operatorCommand("operator"),
		rt.plaudCommand("plaud"),
		rt.envbridgeCommand(),
		rt.claudeGuardsCommand("claude-guards"),
		rt.lokCommand("lok"),
		rt.memoCommand("memo"),
		rt.postaCommand("posta"),
		rt.repolicyCommand("repolicy"),
		rt.configCommand(),
		rt.browseCommand(),
	)
	return root, nil
}

func (rt *runtime) catalogCommand() *cobra.Command {
	command := &cobra.Command{Use: "catalog", Short: "Inspect available packages and groups"}
	list := &cobra.Command{
		Use:   "list",
		Short: "List catalog items and groups",
		RunE: func(*cobra.Command, []string) error {
			if rt.json {
				return writeJSON(rt.stdout, rt.catalog)
			}
			if _, err := fmt.Fprintln(rt.stdout, "ITEMS"); err != nil {
				return err
			}
			for _, item := range rt.catalog.Items {
				if _, err := fmt.Fprintf(rt.stdout, "  %-12s %-12s %-12s %s\n", item.ID, item.Kind, item.Status, item.Description); err != nil {
					return err
				}
			}
			if _, err := fmt.Fprintln(rt.stdout, "\nGROUPS"); err != nil {
				return err
			}
			for _, group := range rt.catalog.Groups {
				if _, err := fmt.Fprintf(rt.stdout, "  %-12s %s\n      %s\n", group.ID, strings.Join(group.Items, ", "), group.Description); err != nil {
					return err
				}
			}
			return nil
		},
	}
	command.AddCommand(list)
	return command
}

func (rt *runtime) installCommand() *cobra.Command {
	var agent, scope, binDir, rootDir string
	var dryRun bool
	command := &cobra.Command{
		Use:   "install [item-or-group...]",
		Short: "Install selected packages; defaults to recommended",
		Args:  cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, selectors []string) error {
			return rt.install(selectors, installer.Options{
				Agent: installer.Agent(agent), Scope: installer.Scope(scope), BinDir: binDir, RootDir: rootDir, DryRun: dryRun,
			})
		},
	}
	addInstallFlags(command, &agent, &scope, &binDir, &rootDir, &dryRun)
	return command
}

func (rt *runtime) install(selectors []string, options installer.Options) error {
	items, err := rt.catalog.Resolve(selectors)
	if err != nil {
		return err
	}
	operations, err := rt.installer.Plan(items, options)
	if err != nil {
		return err
	}
	if err := rt.installer.Apply(operations, options.DryRun); err != nil {
		return err
	}
	return rt.printOperations(operations, options.DryRun, "installed", "would install")
}

func (rt *runtime) uninstallCommand() *cobra.Command {
	var agent, scope, binDir, rootDir string
	var dryRun bool
	command := &cobra.Command{
		Use:   "uninstall <item-or-group...>",
		Short: "Remove selected Výbava-managed packages",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, selectors []string) error {
			items, err := rt.catalog.Resolve(selectors)
			if err != nil {
				return err
			}
			operations, err := rt.installer.Plan(items, installer.Options{
				Agent: installer.Agent(agent), Scope: installer.Scope(scope), BinDir: binDir, RootDir: rootDir,
			})
			if err != nil {
				return err
			}
			for index := range operations {
				operations[index].Action = "remove " + operations[index].Kind
			}
			if err := rt.installer.Remove(operations, dryRun); err != nil {
				return err
			}
			return rt.printOperations(operations, dryRun, "removed", "would remove")
		},
	}
	addInstallFlags(command, &agent, &scope, &binDir, &rootDir, &dryRun)
	return command
}

func (rt *runtime) updateCommand() *cobra.Command {
	var dryRun bool
	command := &cobra.Command{
		Use:   "update [item...]",
		Short: "Refresh installed packages from the current Výbava release",
		Args:  cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, selectors []string) error {
			current, err := rt.installer.Store.Load()
			if err != nil {
				return err
			}
			filter := make(map[string]struct{})
			if len(selectors) > 0 {
				items, err := rt.catalog.Resolve(selectors)
				if err != nil {
					return err
				}
				for _, item := range items {
					filter[item.ID] = struct{}{}
				}
			}
			var operations []installer.Operation
			for _, installed := range current.Installed {
				if len(filter) > 0 {
					if _, selected := filter[installed.ItemID]; !selected {
						continue
					}
				}
				operations = append(operations, installer.Operation{
					ItemID: installed.ItemID, Kind: installed.Kind, Agent: installed.Agent,
					Scope: installed.Scope, Destination: installed.Destination, Action: "refresh " + installed.Kind,
				})
			}
			if len(operations) == 0 {
				return errors.New("no matching installed packages")
			}
			if err := rt.installer.Apply(operations, dryRun); err != nil {
				return err
			}
			return rt.printOperations(operations, dryRun, "updated", "would update")
		},
	}
	command.Flags().BoolVar(&dryRun, "dry-run", false, "show the update plan without changing files")
	return command
}

func (rt *runtime) doctorCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check the catalog, installation state, PATH, and runtime prerequisites",
		RunE: func(*cobra.Command, []string) error {
			report := doctor.Run(rt.catalog, rt.installer.Store)
			if rt.json {
				if err := writeJSON(rt.stdout, report); err != nil {
					return err
				}
			} else {
				if _, err := fmt.Fprint(rt.stdout, doctor.FormatText(report)); err != nil {
					return err
				}
			}
			if !report.Healthy() {
				return ErrFindings
			}
			return nil
		},
	}
}

func (rt *runtime) memoryCommand() *cobra.Command {
	command := &cobra.Command{Use: "memory", Short: "Work with AI memory files"}
	command.AddCommand(rt.memoryLintCommand("lint [memory-home...]"))
	rt.addMemoryActions(command)
	return command
}

// addMemoryActions attaches the write-side commands shared by `vybava memory`
// and the memorylint applet.
func (rt *runtime) addMemoryActions(command *cobra.Command) {
	command.AddCommand(rt.memoryFixCommand(), rt.memoryNewCommand(), rt.memoryReindexCommand(),
		rt.memoryGraphCommand(), rt.memoryRefsCommand(), rt.memoryHookCommand())
}

func (rt *runtime) memoryFixCommand() *cobra.Command {
	var dryRun bool
	command := &cobra.Command{
		Use:   "fix [memory-home...]",
		Short: "Normalize notes onto the flat v2 schema",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, homes []string) error {
			changed, failures, err := memorylint.Fix(homes, dryRun)
			if err != nil {
				return err
			}
			if rt.json {
				if err := writeJSON(rt.stdout, map[string]any{"changed": changed, "failures": failures, "dryRun": dryRun}); err != nil {
					return err
				}
			} else {
				verb := "FIXED"
				if dryRun {
					verb = "WOULD FIX"
				}
				for _, path := range changed {
					if _, err := fmt.Fprintf(rt.stdout, "%s %s\n", verb, path); err != nil {
						return err
					}
				}
				for _, failure := range failures {
					if _, err := fmt.Fprintf(rt.stderr, "SKIPPED %s\n", failure); err != nil {
						return err
					}
				}
				if _, err := fmt.Fprintf(rt.stdout, "memorylint: %d note(s) changed, %d failure(s)\n", len(changed), len(failures)); err != nil {
					return err
				}
			}
			if len(failures) > 0 {
				return ErrFindings
			}
			return nil
		},
	}
	command.Flags().BoolVar(&dryRun, "dry-run", false, "report without writing")
	return command
}

func (rt *runtime) memoryNewCommand() *cobra.Command {
	var home, noteType, name, description string
	var provisional bool
	command := &cobra.Command{
		Use:   "new",
		Short: "Scaffold a note that already satisfies the schema",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			path, err := memorylint.NewNote(home, noteType, name, description, provisional)
			if err != nil {
				return err
			}
			if rt.json {
				return writeJSON(rt.stdout, map[string]any{"path": path})
			}
			_, err = fmt.Fprintln(rt.stdout, path)
			return err
		},
	}
	command.Flags().StringVar(&home, "home", ".", "memory home to create the note in")
	command.Flags().StringVar(&noteType, "type", "", "user, feedback, project or reference")
	command.Flags().StringVar(&name, "name", "", "note slug, which is also its filename stem")
	command.Flags().StringVar(&description, "description", "", "one-line trigger description")
	command.Flags().BoolVar(&provisional, "provisional", false, "born provisional: status provisional with expires 60 days out")
	return command
}

func (rt *runtime) memoryReindexCommand() *cobra.Command {
	var write bool
	var teamIndex string
	command := &cobra.Command{
		Use:   "reindex <memory-home>",
		Short: "Render MEMORY.md deterministically from the notes in a home",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			rendered, err := memorylint.Reindex(args[0], teamIndex, write)
			if err != nil {
				return err
			}
			if rt.json {
				return writeJSON(rt.stdout, map[string]any{"index": string(rendered), "written": write})
			}
			if write {
				_, err = fmt.Fprintf(rt.stdout, "memorylint: wrote %s\n", filepath.Join(args[0], "MEMORY.md"))
				return err
			}
			_, err = rt.stdout.Write(rendered)
			return err
		},
	}
	command.Flags().BoolVar(&write, "write", false, "write MEMORY.md instead of printing it")
	command.Flags().StringVar(&teamIndex, "team-index", "", "path of the companion team index to route readers to")
	return command
}

func (rt *runtime) memoryGraphCommand() *cobra.Command {
	var similar bool
	command := &cobra.Command{
		Use:   "graph [memory-home...]",
		Short: "Print the wikilink graph, or likely duplicate pairs",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(_ *cobra.Command, homes []string) error {
			if rt.json {
				report, err := memorylint.GraphData(homes, similar)
				if err != nil {
					return err
				}
				return writeJSON(rt.stdout, report)
			}
			rendered, err := memorylint.Graph(homes, similar)
			if err != nil {
				return err
			}
			_, err = fmt.Fprint(rt.stdout, rendered)
			return err
		},
	}
	command.Flags().BoolVar(&similar, "similar", false, "report likely duplicates instead of the graph")
	return command
}

func (rt *runtime) memoryRefsCommand() *cobra.Command {
	opts := memorylint.DefaultRefOptions()
	var failOn string
	command := &cobra.Command{
		Use:   "refs <memory-home> <file>...",
		Short: "Find references to notes that no longer exist, from outside the home",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			report, err := memorylint.Refs(args[0], args[1:], opts)
			if err != nil {
				return err
			}
			if rt.json {
				if err := writeJSON(rt.stdout, report); err != nil {
					return err
				}
			} else if _, err := fmt.Fprint(rt.stdout, memorylint.FormatText(report)); err != nil {
				return err
			}
			switch failOn {
			case "error":
				if report.Errors() > 0 {
					return ErrFindings
				}
			case "never":
			default:
				return fmt.Errorf("invalid --fail-on %q: use error or never", failOn)
			}
			return nil
		},
	}
	command.Flags().StringVar(&opts.Prefix, "prefix", opts.Prefix, "repo-relative path the home is referenced by")
	command.Flags().BoolVar(&opts.Bare, "bare", false, "also flag unqualified <note>.md names")
	command.Flags().StringVar(&failOn, "fail-on", "error", "error or never")
	return command
}

func (rt *runtime) memoryHookCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "hook",
		Short: "Run as a Claude Code or Codex pre/post-write hook",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			decision := memorylint.RunHook(rt.stdin)
			if !decision.Block {
				return nil
			}
			if _, err := fmt.Fprintln(rt.stderr, "memorylint "+decision.Message); err != nil {
				return err
			}
			return ErrHookBlocked
		},
	}
}

func (rt *runtime) fontfreezeApplet() *cobra.Command {
	command := rt.fontfreezeCommand("fontfreeze [fonts.yaml]")
	command.SilenceUsage = true
	command.SilenceErrors = true
	command.SetOut(rt.stdout)
	command.SetErr(rt.stderr)
	command.PersistentFlags().BoolVar(&rt.json, "json", false, "emit stable JSON output")
	return command
}

func (rt *runtime) ingressgenApplet() *cobra.Command {
	command := rt.ingressgenCommand("ingressgen")
	command.SilenceUsage = true
	command.SilenceErrors = true
	command.SetOut(rt.stdout)
	command.SetErr(rt.stderr)
	command.PersistentFlags().BoolVar(&rt.json, "json", false, "emit stable JSON output")
	return command
}

func (rt *runtime) ingressgenCommand(use string) *cobra.Command {
	command := &cobra.Command{Use: use, Short: "Render default-deny DOCKER-USER rules from a manifest"}
	var outputPath string
	type renderResult struct {
		Status   string `json:"status"`
		Manifest string `json:"manifest"`
		Output   string `json:"output,omitempty"`
		Written  bool   `json:"written"`
		Rules    string `json:"rules,omitempty"`
	}
	render := &cobra.Command{
		Use:   "render <manifest>",
		Short: "Render an iptables-restore ruleset",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			manifest, err := ingressgen.Load(args[0])
			if err != nil {
				return err
			}
			rules, err := ingressgen.Render(manifest)
			if err != nil {
				return err
			}
			if outputPath == "" {
				if rt.json {
					return writeJSON(rt.stdout, renderResult{Status: "ok", Manifest: args[0], Rules: string(rules)})
				}
				_, err = rt.stdout.Write(rules)
				return err
			}
			if err := os.WriteFile(outputPath, rules, 0o644); err != nil {
				return err
			}
			if rt.json {
				return writeJSON(rt.stdout, renderResult{Status: "ok", Manifest: args[0], Output: outputPath, Written: true})
			}
			_, err = fmt.Fprintf(rt.stdout, "wrote %s\n", outputPath)
			return err
		},
	}
	render.Flags().StringVarP(&outputPath, "output", "o", "", "write rendered rules to this path")

	check := &cobra.Command{
		Use:   "check <manifest> <rendered-rules>",
		Short: "Fail when committed rules drift from the manifest",
		Args:  cobra.ExactArgs(2),
		RunE: func(_ *cobra.Command, args []string) error {
			manifest, err := ingressgen.Load(args[0])
			if err != nil {
				return err
			}
			rules, err := ingressgen.Render(manifest)
			if err != nil {
				return err
			}
			existing, err := os.ReadFile(args[1])
			if err != nil {
				return err
			}
			if err := ingressgen.Check(rules, existing); err != nil {
				return err
			}
			if rt.json {
				return writeJSON(rt.stdout, map[string]any{"status": "ok", "manifest": args[0], "rules": args[1]})
			}
			_, err = fmt.Fprintf(rt.stdout, "ok: %s matches %s\n", args[1], args[0])
			return err
		},
	}
	var restorePath string
	apply := &cobra.Command{
		Use:   "apply <manifest>",
		Short: "Syntax-check and apply without flushing unrelated filter chains",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			manifest, err := ingressgen.Load(args[0])
			if err != nil {
				return err
			}
			rules, err := ingressgen.Render(manifest)
			if err != nil {
				return err
			}
			if err := ingressgen.Apply(cmd.Context(), restorePath, rules); err != nil {
				return err
			}
			if rt.json {
				return writeJSON(rt.stdout, map[string]string{"status": "ok", "manifest": args[0]})
			}
			_, err = fmt.Fprintf(rt.stdout, "applied %s with --noflush\n", args[0])
			return err
		},
	}
	apply.Flags().StringVar(&restorePath, "iptables-restore", "/usr/sbin/iptables-restore", "iptables-restore binary (tests and nonstandard hosts)")
	command.AddCommand(render, check, apply)
	return command
}

func (rt *runtime) fontfreezeCommand(use string) *cobra.Command {
	var dryRun bool
	command := &cobra.Command{
		Use:   use,
		Short: "Freeze variable webfonts at the styles a site renders and subset them per language",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			manifestPath := "fonts.yaml"
			if len(args) == 1 {
				manifestPath = args[0]
			}
			manifest, err := fontfreeze.LoadManifest(manifestPath)
			if err != nil {
				return err
			}
			jobs, err := fontfreeze.Plan(manifest, filepath.Dir(manifestPath))
			if err != nil {
				return err
			}
			if dryRun {
				if rt.json {
					return writeJSON(rt.stdout, jobs)
				}
				for _, job := range jobs {
					fmt.Fprintf(rt.stdout, "%s <- %s %v\n", job.Output, job.Master, job.InstancerArgs)
				}
				return nil
			}
			if err := fontfreeze.CheckTooling(); err != nil {
				return err
			}
			fmt.Fprintln(rt.stderr, fontfreeze.LicenseReminder)
			report, err := fontfreeze.Run(jobs, fontfreeze.ExecRunner)
			if err != nil {
				return err
			}
			report.Manifest = manifestPath
			if rt.json {
				return writeJSON(rt.stdout, report)
			}
			_, err = fmt.Fprint(rt.stdout, fontfreeze.FormatText(report))
			return err
		},
	}
	command.Flags().BoolVar(&dryRun, "dry-run", false, "print planned fonttools work without executing")
	return command
}

func (rt *runtime) memorylintApplet() *cobra.Command {
	command := rt.memoryLintCommand("memorylint [memory-home...]")
	command.Short = "Validate AI memory files and Obsidian links"
	command.SilenceUsage = true
	command.SilenceErrors = true
	command.SetOut(rt.stdout)
	command.SetErr(rt.stderr)
	command.PersistentFlags().BoolVar(&rt.json, "json", false, "emit stable JSON output")
	command.AddCommand(rt.memoryLintCommand("check [memory-home...]"))
	rt.addMemoryActions(command)
	return command
}

func (rt *runtime) memoryLintCommand(use string) *cobra.Command {
	var failOn string
	command := &cobra.Command{
		Use:   use,
		Short: "Lint memory homes",
		Args:  cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, paths []string) error {
			report, err := memorylint.Lint(paths)
			if err != nil {
				return err
			}
			if rt.json {
				if err := writeJSON(rt.stdout, report); err != nil {
					return err
				}
			} else {
				if _, err := fmt.Fprint(rt.stdout, memorylint.FormatText(report)); err != nil {
					return err
				}
			}
			switch failOn {
			case "warning":
				if len(report.Findings) > 0 {
					return ErrFindings
				}
			case "error":
				if report.Errors() > 0 {
					return ErrFindings
				}
			case "never":
			default:
				return fmt.Errorf("invalid --fail-on %q: use warning, error, or never", failOn)
			}
			return nil
		},
	}
	command.Flags().StringVar(&failOn, "fail-on", "warning", "minimum finding severity that produces a non-zero exit (warning, error, never)")
	return command
}

// perfrigCommand wires the reusable performance-drill orchestrator. Domain
// logic lives in internal/perfrig; this is thin CLI glue per the architecture
// laws. Subcommands: validate, plan (dry look, runs nothing), run.
func (rt *runtime) perfrigCommand(use string) *cobra.Command {
	command := &cobra.Command{
		Use:   use,
		Short: "Run a performance drill from a testing/<project>/perf manifest",
		Long: "perfrig orchestrates the generic, safety-critical half of a load test — the neighbor guard,\n" +
			"the staged ramp (push-to-first-failure), and the percentile-vs-concurrency report — while each\n" +
			"project supplies its own seed/auth/generator commands via a perf.manifest.yml.",
	}

	validate := &cobra.Command{
		Use:   "validate <manifest>",
		Short: "Parse and validate a perf manifest",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			m, err := perfrig.LoadManifest(args[0])
			if err != nil {
				return err
			}
			if rt.json {
				return writeJSON(rt.stdout, m)
			}
			fmt.Fprintf(rt.stdout, "ok: %s (mode=%s, %d generator(s), %d stage(s))\n",
				m.Project, m.Mode, len(m.Generators), len(m.Ramp.Stages))
			return nil
		},
	}

	plan := &cobra.Command{
		Use:   "plan <manifest>",
		Short: "Print the resolved drill (stages + commands) without running anything",
		Args:  cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			m, err := perfrig.LoadManifest(args[0])
			if err != nil {
				return err
			}
			if rt.json {
				return writeJSON(rt.stdout, struct {
					Manifest perfrig.Manifest    `json:"manifest"`
					Stages   []perfrig.StagePlan `json:"stages"`
				}{m, perfrig.PlanRamp(m)})
			}
			fmt.Fprint(rt.stdout, perfrig.Runner{M: m}.Plan())
			return nil
		},
	}

	var maxStage int
	var reportOut string
	run := &cobra.Command{
		Use:   "run <manifest>",
		Short: "Execute the drill: seed → (rig up) → guarded ramp → report",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := perfrig.LoadManifest(args[0])
			if err != nil {
				return err
			}
			// Under --json the live stream moves to stderr so stdout carries
			// exactly one machine-readable document: the drill object.
			streamOut := rt.stdout
			if rt.json {
				streamOut = rt.stderr
			}
			runner := perfrig.Runner{M: m, Opt: perfrig.Options{
				Stdout:   streamOut,
				Stderr:   rt.stderr,
				MaxStage: maxStage,
			}}
			drill, err := runner.Run(cmd.Context())
			if err != nil {
				return err
			}
			report := drill.Markdown()
			if rt.json {
				if jerr := writeJSON(rt.stdout, drill); jerr != nil {
					return jerr
				}
			} else {
				fmt.Fprint(rt.stdout, "\n"+report)
			}
			targets := []string{}
			if reportOut != "" {
				targets = append(targets, reportOut)
			}
			// The manifest's report.out is a directory; each run drops a
			// timestamped artifact there.
			if m.Report.Out != "" {
				dir, derr := expandHome(m.Report.Out)
				if derr != nil {
					return derr
				}
				if derr := os.MkdirAll(dir, 0o755); derr != nil {
					return fmt.Errorf("report.out %q must be a writable directory: %w", dir, derr)
				}
				targets = append(targets, filepath.Join(dir,
					fmt.Sprintf("%s-%s.md", m.Project, drill.StartedAt.Format("20060102-150405"))))
			}
			for _, t := range targets {
				if werr := os.WriteFile(t, []byte(report), 0o644); werr != nil {
					return werr
				}
				fmt.Fprintf(rt.stderr, "report written to %s\n", t)
			}
			return nil
		},
	}
	run.Flags().IntVar(&maxStage, "max-stage", 0, "run only the first N stages (0 = whole ramp); use for calibration")
	run.Flags().StringVar(&reportOut, "report-out", "", "also write the markdown report to this path")

	command.AddCommand(validate, plan, run)
	return command
}

func (rt *runtime) browseCommand() *cobra.Command {
	var agent, scope, binDir, rootDir string
	var dryRun bool
	command := &cobra.Command{
		Use:   "browse",
		Short: "Interactively select and install catalog packages",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			if rt.json {
				return errors.New("browse is interactive; use install --json for automation")
			}
			ids, confirmed, err := ui.Select(rt.catalog)
			if err != nil {
				return err
			}
			if !confirmed {
				_, err := fmt.Fprintln(rt.stdout, "No changes made.")
				return err
			}
			if len(ids) == 0 {
				return errors.New("no packages selected")
			}
			return rt.install(ids, installer.Options{
				Agent: installer.Agent(agent), Scope: installer.Scope(scope), BinDir: binDir, RootDir: rootDir, DryRun: dryRun,
			})
		},
	}
	addInstallFlags(command, &agent, &scope, &binDir, &rootDir, &dryRun)
	return command
}

func (rt *runtime) printOperations(operations []installer.Operation, dryRun bool, past, future string) error {
	if rt.json {
		return writeJSON(rt.stdout, map[string]any{"dry_run": dryRun, "operations": operations})
	}
	verb := past
	if dryRun {
		verb = future
	}
	for _, operation := range operations {
		target := operation.Destination
		if operation.Agent != "" {
			target = operation.Agent + ":" + target
		}
		if _, err := fmt.Fprintf(rt.stdout, "%s %s → %s\n", verb, operation.ItemID, target); err != nil {
			return err
		}
	}
	return nil
}

func addInstallFlags(command *cobra.Command, agent, scope, binDir, rootDir *string, dryRun *bool) {
	command.Flags().StringVar(agent, "agent", "all", "skill target: claude, codex, or all")
	command.Flags().StringVar(scope, "scope", "user", "installation scope: user or project")
	command.Flags().StringVar(binDir, "bin-dir", "", "applet destination (default ~/.local/bin)")
	command.Flags().StringVar(rootDir, "root", "", "project root for project-scoped installs (default current directory)")
	command.Flags().BoolVar(dryRun, "dry-run", false, "show the installation plan without changing files")
}

// expandHome resolves a leading "~/" (or bare "~") against the user's home
// directory so manifest paths like ~/Exports/... land where they promise.
func expandHome(path string) (string, error) {
	if path != "~" && !strings.HasPrefix(path, "~/") {
		return path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, strings.TrimPrefix(path, "~")), nil
}

func writeJSON(writer io.Writer, value any) error {
	encoder := json.NewEncoder(writer)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func KnownApplet(invokedAs string) bool {
	return filepath.Base(invokedAs) == "memorylint"
}

func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var coder runx.ExitCoder
	if errors.As(err, &coder) {
		return coder.ExitCode()
	}
	if errors.Is(err, ErrFindings) {
		return 1
	}
	return 2
}

func ErrorText(err error) string {
	if err == nil || errors.Is(err, ErrFindings) || errors.Is(err, ErrHookBlocked) {
		return ""
	}
	return err.Error()
}

func SortedItemIDs(c catalog.Catalog) []string {
	ids := make([]string, 0, len(c.Items))
	for _, item := range c.Items {
		ids = append(ids, item.ID)
	}
	sort.Strings(ids)
	return ids
}

func (rt *runtime) codexsyncApplet() *cobra.Command {
	command := rt.codexsyncCommand("codexsync")
	command.SilenceUsage = true
	command.SilenceErrors = true
	command.SetOut(rt.stdout)
	command.SetErr(rt.stderr)
	command.PersistentFlags().BoolVar(&rt.json, "json", false, "emit stable JSON output")
	return command
}

// codexsyncConfig resolves the four homes, letting each be overridden so a
// test or a second machine profile can render somewhere else entirely.
func codexsyncConfig(claudeHome, agentsHome, codexHome, backupRoot string) (codexsync.Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return codexsync.Config{}, err
	}
	pick := func(override, fallback string) string {
		if override != "" {
			return override
		}
		return filepath.Join(home, fallback)
	}
	cfg := codexsync.Config{
		ClaudeHome: pick(claudeHome, ".claude"),
		AgentsHome: pick(agentsHome, ".agents"),
		CodexHome:  pick(codexHome, ".codex"),
		BackupRoot: pick(backupRoot, "Backups"),
	}
	for _, field := range []*string{&cfg.ClaudeHome, &cfg.AgentsHome, &cfg.CodexHome, &cfg.BackupRoot} {
		*field, err = filepath.Abs(*field)
		if err != nil {
			return codexsync.Config{}, err
		}
	}
	return cfg, nil
}

func (rt *runtime) codexsyncCommand(use string) *cobra.Command {
	command := &cobra.Command{
		Use:   use,
		Short: "Render Claude skills and commands into the structure Codex discovers",
		Long: "Codex does not load Claude command files as skills. codexsync renders\n" +
			"~/.claude/skills and ~/.claude/commands\n" +
			"into ~/.agents/skills — Codex's documented user scope — deterministically:\n" +
			"skills keep their nesting, each command becomes a source-command-<slug> skill,\n" +
			"and a disable-model-invocation opt-out crosses over as the Codex policy\n" +
			"sidecar. It also owns the [[skills.config]] block that suppresses duplicate\n" +
			"discovery, and prunes prompt entries Codex cannot read. Displaced files are\n" +
			"copied under ~/Backups/codexsync/ first.",
	}
	var claudeHome, agentsHome, codexHome, backupRoot string
	var dryRun bool

	command.PersistentFlags().StringVar(&claudeHome, "claude-home", "", "source Claude home (default ~/.claude)")
	command.PersistentFlags().StringVar(&agentsHome, "agents-home", "", "destination agents home (default ~/.agents)")
	command.PersistentFlags().StringVar(&codexHome, "codex-home", "", "Codex home holding config.toml (default ~/.codex)")
	command.PersistentFlags().StringVar(&backupRoot, "backup-root", "", "where displaced files are copied (default ~/Backups)")

	type planResult struct {
		Status       string            `json:"status"`
		Skills       int               `json:"skills"`
		Commands     int               `json:"commands"`
		Suppress     []string          `json:"suppress"`
		StalePrompts []string          `json:"stalePrompts"`
		Entries      []codexsync.Entry `json:"entries"`
		Changes      *codexsync.Report `json:"changes"`
	}

	plan := &cobra.Command{
		Use:   "plan",
		Short: "Show what would be rendered, touching nothing",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := codexsyncConfig(claudeHome, agentsHome, codexHome, backupRoot)
			if err != nil {
				return err
			}
			p, err := codexsync.BuildPlan(cfg)
			if err != nil {
				return err
			}
			changes, err := codexsync.Apply(cfg, p, true)
			if err != nil {
				return err
			}
			skills, commands := 0, 0
			for _, entry := range p.Entries {
				if entry.Kind == "command" {
					commands++
					continue
				}
				skills++
			}
			if rt.json {
				return writeJSON(rt.stdout, planResult{
					Status: "ok", Skills: skills, Commands: commands,
					Suppress: p.Suppress, StalePrompts: p.StalePrompts,
					Entries: p.Entries, Changes: changes,
				})
			}
			_, err = fmt.Fprintf(rt.stdout,
				"%d skills, %d commands -> %s\n%d duplicate paths suppressed, %d unreadable prompt entries\n",
				skills, commands, filepath.Join(cfg.AgentsHome, "skills"), len(p.Suppress), len(p.StalePrompts))
			if err != nil {
				return err
			}
			_, err = fmt.Fprintf(rt.stdout, "would write %d, remove %d, unchanged %d; config changed %t, manifest changed %t\n",
				len(changes.Written), len(changes.Removed), changes.Unchanged, changes.Config != "", changes.Manifest)
			return err
		},
	}

	apply := &cobra.Command{
		Use:   "apply",
		Short: "Render the tree, prune what it no longer owns, update config.toml",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := codexsyncConfig(claudeHome, agentsHome, codexHome, backupRoot)
			if err != nil {
				return err
			}
			p, err := codexsync.BuildPlan(cfg)
			if err != nil {
				return err
			}
			report, err := codexsync.Apply(cfg, p, dryRun)
			if err != nil {
				return err
			}
			if rt.json {
				return writeJSON(rt.stdout, report)
			}
			_, err = fmt.Fprintf(rt.stdout, "written %d, removed %d, unchanged %d; config changed %t, manifest changed %t\n",
				len(report.Written), len(report.Removed), report.Unchanged, report.Config != "", report.Manifest)
			if err != nil || report.Backup == "" {
				return err
			}
			_, err = fmt.Fprintf(rt.stdout, "backup %s\n", report.Backup)
			return err
		},
	}
	apply.Flags().BoolVar(&dryRun, "dry-run", false, "report the changes without writing them")

	check := &cobra.Command{
		Use:   "check",
		Short: "Fail when the rendered tree has drifted from the Claude home",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cfg, err := codexsyncConfig(claudeHome, agentsHome, codexHome, backupRoot)
			if err != nil {
				return err
			}
			p, err := codexsync.BuildPlan(cfg)
			if err != nil {
				return err
			}
			if err := codexsync.Check(cfg, p); err != nil {
				return err
			}
			if rt.json {
				return writeJSON(rt.stdout, map[string]string{"status": "ok"})
			}
			_, err = fmt.Fprintln(rt.stdout, "in sync")
			return err
		},
	}

	command.AddCommand(plan, apply, check)
	// Drift and operational errors must remain machine-readable on stdout.
	for _, child := range command.Commands() {
		run := child.RunE
		child.RunE = func(cmd *cobra.Command, args []string) error {
			err := run(cmd, args)
			if err != nil && rt.json {
				return errors.Join(err, writeJSON(rt.stdout, map[string]string{"status": "error", "error": err.Error()}))
			}
			return err
		}
	}
	return command
}

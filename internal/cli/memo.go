package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/memo"
	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/spf13/cobra"
)

func (rt *runtime) memoApplet() *cobra.Command {
	cmd := rt.memoCommand("memo")
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(rt.stdout)
	cmd.SetErr(rt.stderr)
	cmd.PersistentFlags().BoolVar(&rt.json, "json", false, "emit machine-readable output")
	return cmd
}

// memoEnv builds the process context every memo verb resolves homes from.
func memoEnv() memo.Env {
	home, _ := os.UserHomeDir()
	return memo.Env{UserHome: home, Cwd: workingDir(), Session: os.Getenv("CLAUDE_CODE_SESSION_ID")}
}

// memoCommand wires the ledger verbs. Every verb: one envelope, the closed
// memo.Diag codes, `next` naming the follow-up command.
func (rt *runtime) memoCommand(use string) *cobra.Command {
	root := &cobra.Command{
		Use:   use,
		Short: "Append-only memory ledger - capture a row, cite it, render the hot surface",
		Long: "memo owns LEDGER.md (append-only truth), MEMORY.md (rendered) and usage.jsonl.\n" +
			"  memo add <type>/<topic>[!] \"<sentence>.\" [--link <l>]... [--supersedes N] [--retires N]\n" +
			"  memo show <ref> · memo find <words>... · memo touch <ref> · memo render [--check] · memo ensure\n" +
			"  memo import <file> · memo migrate <home> · memo homes [register <alias> <path>]\n" +
			"  memo vault [--path ~/Memory] · memo snapshot [-m msg] · memo log [-n N] · memo restore <rev> <file>\n" +
			"  memo hook  (Claude Code PreToolUse + Stop payload on stdin)",
	}
	var homeSpec string
	root.PersistentFlags().StringVar(&homeSpec, "home", "", "home alias or path (default: resolved from cwd)")

	session := func(cmd *cobra.Command) *runx.Session {
		return &runx.Session{Tool: "memo", JSON: rt.json, Verb: cmd.Name(), Stdout: rt.stdout, Stderr: rt.stderr}
	}
	finish := func(s *runx.Session, data any, next []string, warnings []*memo.Diag, err error) error {
		env := runx.Envelope{OK: err == nil, Verb: s.Verb, Data: data, Diagnostics: []runx.Diagnostic{}, Next: next}
		if env.Next == nil {
			env.Next = []string{}
		}
		for _, w := range warnings {
			if w != nil {
				env.Diagnostics = append(env.Diagnostics, runx.Diagnostic{Code: w.Code, Severity: w.Severity, Detail: w.Detail, Fix: w.Fix})
			}
		}
		var d *memo.Diag
		if errors.As(err, &d) {
			diag := runx.Diagnostic{Code: d.Code, Severity: d.Severity, Detail: d.Detail, Fix: d.Fix}
			env.Diagnostics = append(env.Diagnostics, diag)
			if d.Fix != "" {
				env.Next = append(env.Next, d.Fix)
			}
			if d.Severity == "info" {
				env.OK, err = true, nil
			} else {
				err = &runx.DiagError{Diag: diag}
			}
		} else if err != nil {
			env.Diagnostics = append(env.Diagnostics, runx.Diagnostic{Code: runx.DiagInfraError, Severity: "error", Detail: err.Error()})
		}
		if emitErr := s.Emit(env); emitErr != nil {
			return emitErr
		}
		if code := s.Finish(err); code != 0 {
			return runx.ExitError{Code: code}
		}
		return nil
	}
	// finishMany emits several error diagnostics in one envelope (import
	// reports every bad line at once); exit 2.
	finishMany := func(s *runx.Session, ds []*memo.Diag) error {
		env := runx.Envelope{OK: false, Verb: s.Verb, Diagnostics: []runx.Diagnostic{}, Next: []string{}}
		for _, d := range ds {
			env.Diagnostics = append(env.Diagnostics, runx.Diagnostic{Code: d.Code, Severity: d.Severity, Detail: d.Detail, Fix: d.Fix})
		}
		env.Next = append(env.Next, ds[0].Fix)
		if err := s.Emit(env); err != nil {
			return err
		}
		return runx.ExitError{Code: 2}
	}
	usage := func(detail, fix string) error {
		return &memo.Diag{Code: memo.DiagUsage, Severity: "error", Detail: detail, Fix: fix}
	}
	homeArg := func(h memo.Home) string { return " --home " + h.Path }
	root.Args = func(cmd *cobra.Command, args []string) error {
		if len(args) == 0 {
			return nil
		}
		return finish(session(cmd), nil, nil, nil, usage(fmt.Sprintf("%q is not a memo verb", args[0]), "memo --help"))
	}
	root.RunE = func(cmd *cobra.Command, _ []string) error { return cmd.Help() }

	// add
	var links []string
	var supersedes, retires int
	add := &cobra.Command{Use: "add <type>/<topic>[!] \"<sentence>.\"", Short: "Append one row, render, snapshot the personal home", Args: cobra.ArbitraryArgs}
	add.RunE = func(cmd *cobra.Command, args []string) error {
		s := session(cmd)
		if len(args) != 2 {
			return finish(s, nil, nil, nil, usage("add takes exactly <type>/<topic>[!] and one quoted sentence", "memo add feedback/git \"Never git stash; parallel sessions share the tree.\""))
		}
		typ, topic, pinned, d := memo.ParseHead(args[0])
		if d != nil {
			return finish(s, nil, nil, nil, d)
		}
		sentence := args[1]
		prefix := ""
		if memo.TypeKind[typ] == memo.KindTeam {
			prefix = "t"
		}
		if supersedes > 0 && !strings.HasPrefix(sentence, "supersedes #") {
			sentence = fmt.Sprintf("supersedes #%s%d: %s", prefix, supersedes, sentence)
		}
		if retires > 0 && !strings.HasPrefix(sentence, "retires #") {
			sentence = fmt.Sprintf("retires #%s%d. %s", prefix, retires, sentence)
		}
		env := memoEnv()
		homes, d, err := env.Resolve(homeSpec, typ)
		if d != nil || err != nil {
			return finish(s, nil, nil, nil, diagOrErr(d, err))
		}
		l, d, err := env.Open(homes[0], true)
		if d != nil || err != nil {
			return finish(s, nil, nil, nil, diagOrErr(d, err))
		}
		now := time.Now()
		row, d, err := l.Append(memo.Row{Type: typ, Topic: topic, Pinned: pinned, Sentence: sentence, Links: links})
		if d != nil || err != nil {
			return finish(s, nil, nil, nil, diagOrErr(d, err))
		}
		events, err := recordAdded(env, l.Home(), []memo.Row{row}, now)
		if err != nil {
			return finish(s, nil, nil, nil, err)
		}
		_, tracked, err := memo.WriteIndex(l, events, now)
		if err != nil {
			return finish(s, nil, nil, nil, err)
		}
		data := map[string]any{"row": row, "line": row.Format(), "home": l.Home(), "cite": row.Cite()}
		if l.Kind == memo.KindPersonal {
			if rev, d, err := memo.Snapshot(homes[0], fmt.Sprintf("memo: snapshot %s (add #%d %s/%s)", l.Alias, row.ID, typ, topic)); err != nil {
				return finish(s, data, nil, nil, err)
			} else if d == nil {
				data["snapshot"] = rev
			}
		}
		return finish(s, data, []string{fmt.Sprintf("memo show %s%d%s --json", row.IDPrefix(), row.ID, homeArg(homes[0]))}, []*memo.Diag{memo.SentenceWarning(sentence), tracked}, nil)
	}
	add.Flags().StringArrayVar(&links, "link", nil, "link after ->: [[notes/<slug>]], [[LEDGER#^m<id>]], [[<alias>/...]], https://...")
	add.Flags().IntVar(&supersedes, "supersedes", 0, "mark row N superseded by this one")
	add.Flags().IntVar(&retires, "retires", 0, "mark row N retired by this one")

	// show
	show := &cobra.Command{Use: "show <ref>", Short: "Print a row and its linked notes; records a show event", Args: cobra.ArbitraryArgs}
	show.RunE = func(cmd *cobra.Command, args []string) error {
		s := session(cmd)
		if len(args) != 1 {
			return finish(s, nil, nil, nil, usage("show takes one reference", "memo show 45"))
		}
		ref, d := memo.ParseRef(args[0])
		if d != nil {
			return finish(s, nil, nil, nil, d)
		}
		env := memoEnv()
		l, row, d, err := env.Locate(ref, homeSpec)
		if d != nil || err != nil {
			return finish(s, nil, nil, nil, diagOrErr(d, err))
		}
		status, by := l.Status(row.ID)
		notes := map[string]string{}
		for _, link := range row.Links {
			if strings.HasPrefix(link, "[[notes/") {
				slug := strings.TrimSuffix(strings.TrimPrefix(link, "[[notes/"), "]]")
				if body, err := os.ReadFile(filepath.Join(l.Home(), memo.NotesDir, slug+".md")); err == nil {
					notes[slug] = string(body)
				}
			}
		}
		if err := recordEvent(env, l.Home(), row.ID, "show"); err != nil {
			return finish(s, nil, nil, nil, err)
		}
		payload := map[string]any{"row": row, "line": row.Format(), "home": l.Home(), "alias": l.Alias, "status": status, "notes": notes}
		var data any = payload
		next := []string{}
		if by > 0 {
			payload["closedBy"] = by
			next = append(next, fmt.Sprintf("memo show %s#%d", l.Alias, by))
		}
		if !rt.json {
			fmt.Fprintln(rt.stdout, row.Format())
			for slug, body := range notes {
				fmt.Fprintf(rt.stdout, "\n--- notes/%s.md ---\n%s", slug, body)
			}
			data = nil
		}
		return finish(s, data, next, nil, nil)
	}

	// find
	var all bool
	find := &cobra.Command{Use: "find <words>...", Short: "Search sentences and topics; superseded rows marked", Args: cobra.ArbitraryArgs}
	find.RunE = func(cmd *cobra.Command, args []string) error {
		s := session(cmd)
		if len(args) == 0 {
			return finish(s, nil, nil, nil, usage("find takes at least one word", "memo find stash"))
		}
		env := memoEnv()
		var homes []memo.Home
		var d *memo.Diag
		var err error
		if all {
			homes, d, err = env.Discover()
		} else {
			homes, d, err = env.Resolve(homeSpec, "")
		}
		if d != nil || err != nil {
			return finish(s, nil, nil, nil, diagOrErr(d, err))
		}
		hits := []map[string]any{}
		for _, h := range homes {
			l, d, err := env.Open(h, false)
			if err != nil {
				return finish(s, nil, nil, nil, err)
			}
			if d != nil {
				continue
			}
			for _, r := range l.Rows {
				if matchesAll(r, args) {
					status, by := l.Status(r.ID)
					hit := map[string]any{"alias": l.Alias, "id": r.ID, "line": r.Format(), "status": status}
					if by > 0 {
						hit["closedBy"] = by
					}
					hits = append(hits, hit)
					if !rt.json {
						mark := ""
						if status != "active" {
							mark = fmt.Sprintf("  [%s by #%d]", status, by)
						}
						fmt.Fprintf(rt.stdout, "%s  %s%s\n", l.Alias, r.Format(), mark)
					}
				}
			}
		}
		next := []string{}
		if len(hits) > 0 {
			next = append(next, fmt.Sprintf("memo show %s#%d --json", hits[0]["alias"], hits[0]["id"]))
		}
		var data any = map[string]any{"hits": hits, "homes": homes}
		if !rt.json {
			data = nil
		}
		return finish(s, data, next, nil, nil)
	}
	find.Flags().BoolVar(&all, "all", false, "search every registered home")

	// touch
	touch := &cobra.Command{Use: "touch <ref>", Short: "Record an explicit use of a row", Args: cobra.ArbitraryArgs}
	touch.RunE = func(cmd *cobra.Command, args []string) error {
		s := session(cmd)
		if len(args) != 1 {
			return finish(s, nil, nil, nil, usage("touch takes one reference", "memo touch 45"))
		}
		ref, d := memo.ParseRef(args[0])
		if d != nil {
			return finish(s, nil, nil, nil, d)
		}
		env := memoEnv()
		l, row, d, err := env.Locate(ref, homeSpec)
		if d != nil || err != nil {
			return finish(s, nil, nil, nil, diagOrErr(d, err))
		}
		if err := recordEvent(env, l.Home(), row.ID, "touch"); err != nil {
			return finish(s, nil, nil, nil, err)
		}
		return finish(s, map[string]any{"row": row.ID, "home": l.Home()}, []string{"memo render --home " + l.Home() + " --json"}, nil, nil)
	}

	// render
	var check bool
	render := &cobra.Command{Use: "render", Short: "Write MEMORY.md from the ledger (--check: exit 2 on drift)", Args: cobra.NoArgs}
	render.RunE = func(cmd *cobra.Command, _ []string) error {
		s := session(cmd)
		env := memoEnv()
		homes, d, err := env.Resolve(homeSpec, "")
		if d != nil || err != nil {
			return finish(s, nil, nil, nil, diagOrErr(d, err))
		}
		now := time.Now()
		results := []map[string]any{}
		var warnings []*memo.Diag
		next := []string{"memo render --check" + flagIf(homeSpec) + " --json"}
		for _, h := range homes {
			l, d, err := env.Open(h, false)
			if d != nil || err != nil {
				return finish(s, nil, nil, nil, diagOrErr(d, err))
			}
			events, d, err := memo.LoadEvents(h.Path)
			if d != nil || err != nil {
				return finish(s, nil, nil, nil, diagOrErr(d, err))
			}
			// A tracked MEMORY.md is left as committed, so --check has no
			// drift to judge and a write has nothing to do: both answer with
			// the untracking fix instead.
			if tracked := memo.TrackedIndex(h.Path, l.Kind); tracked != nil {
				warnings = append(warnings, tracked)
				next = append(next, tracked.Fix)
				results = append(results, map[string]any{"home": h.Path, "tracked": true})
				continue
			}
			if check {
				same, err := memo.CheckIndex(l, events, now)
				if err != nil {
					return finish(s, nil, nil, nil, err)
				}
				if !same {
					return finish(s, map[string]any{"home": h.Path, "drift": true}, nil, nil, &memo.Diag{Code: memo.DiagRenderDrift, Severity: "error", Detail: filepath.Join(h.Path, memo.IndexFile) + " differs from the render", Fix: "memo render --home " + h.Path + " --json"})
				}
				results = append(results, map[string]any{"home": h.Path, "drift": false})
				continue
			}
			changed, _, err := memo.WriteIndex(l, events, now)
			if err != nil {
				return finish(s, nil, nil, nil, err)
			}
			results = append(results, map[string]any{"home": h.Path, "changed": changed, "rows": len(memo.Order(l, events, now))})
		}
		return finish(s, map[string]any{"homes": results}, next, warnings, nil)
	}
	render.Flags().BoolVar(&check, "check", false, "exit 2 when MEMORY.md on disk differs")

	// ensure
	ensure := &cobra.Command{Use: "ensure", Short: "Render MEMORY.md only when missing or older than the ledger (SessionStart)", Args: cobra.NoArgs}
	ensure.RunE = func(cmd *cobra.Command, _ []string) error {
		s := session(cmd)
		env := memoEnv()
		homes, d, err := env.Resolve(homeSpec, "")
		if d != nil || err != nil {
			return finish(s, nil, nil, nil, diagOrErr(d, err))
		}
		now := time.Now()
		results := []memo.EnsureResult{}
		var warnings []*memo.Diag
		next := []string{"memo render --check" + flagIf(homeSpec) + " --json"}
		for _, h := range homes {
			r, d, err := env.EnsureHome(h, now)
			if d != nil || err != nil {
				return finish(s, nil, nil, nil, diagOrErr(d, err))
			}
			if r.Tracked != nil {
				warnings = append(warnings, r.Tracked)
				next = append(next, r.Tracked.Fix)
			}
			results = append(results, r)
		}
		return finish(s, map[string]any{"homes": results}, next, warnings, nil)
	}

	// import
	imp := &cobra.Command{Use: "import <file>", Short: "Append id-less rows from a file, assigning ids in order", Args: cobra.ArbitraryArgs}
	imp.RunE = func(cmd *cobra.Command, args []string) error {
		s := session(cmd)
		if len(args) != 1 {
			return finish(s, nil, nil, nil, usage("import takes one file", "memo import rows.md --home <alias|path>"))
		}
		data, err := os.ReadFile(args[0])
		if err != nil {
			return finish(s, nil, nil, nil, err)
		}
		rows, ds := memo.ParseImport(data)
		if len(ds) > 0 {
			return finishMany(s, ds)
		}
		if len(rows) == 0 {
			return finish(s, nil, nil, nil, usage(args[0]+" holds no rows", "memo migrate <home>  # prints a template"))
		}
		env := memoEnv()
		homes, d, err := env.Resolve(homeSpec, rows[0].Type)
		if d != nil || err != nil {
			return finish(s, nil, nil, nil, diagOrErr(d, err))
		}
		l, d, err := env.Open(homes[0], true)
		if d != nil || err != nil {
			return finish(s, nil, nil, nil, diagOrErr(d, err))
		}
		added, ds, err := l.Import(rows)
		if err != nil {
			return finish(s, nil, nil, nil, err)
		}
		if len(ds) > 0 {
			return finishMany(s, ds)
		}
		now := time.Now()
		events, err := recordAdded(env, l.Home(), added, now)
		if err != nil {
			return finish(s, nil, nil, nil, err)
		}
		_, tracked, err := memo.WriteIndex(l, events, now)
		if err != nil {
			return finish(s, nil, nil, nil, err)
		}
		result := map[string]any{"home": l.Home(), "added": len(added), "first": added[0].ID, "last": added[len(added)-1].ID}
		if l.Kind == memo.KindPersonal {
			if rev, d, err := memo.Snapshot(homes[0], fmt.Sprintf("memo: snapshot %s (import %d rows)", l.Alias, len(added))); err != nil {
				return finish(s, result, nil, nil, err)
			} else if d == nil {
				result["snapshot"] = rev
			}
		}
		return finish(s, result, []string{"memorylint check " + l.Home()}, []*memo.Diag{tracked}, nil)
	}

	// migrate
	migrate := &cobra.Command{Use: "migrate <home>", Short: "List v2 notes and print an import template (helper, not the judgment)", Args: cobra.ArbitraryArgs}
	migrate.RunE = func(cmd *cobra.Command, args []string) error {
		s := session(cmd)
		if len(args) != 1 {
			return finish(s, nil, nil, nil, usage("migrate takes one memory home directory", "memo migrate ~/.claude/projects/<slug>/memory"))
		}
		notes, template, err := memo.Migrate(args[0], time.Now())
		if err != nil {
			return finish(s, nil, nil, nil, err)
		}
		if !rt.json {
			fmt.Fprint(rt.stdout, template)
			return finish(s, nil, []string{"memo import <edited-template> --home " + args[0]}, nil, nil)
		}
		return finish(s, map[string]any{"notes": notes, "template": template}, []string{"memo import <edited-template> --home " + args[0]}, nil, nil)
	}

	// homes
	homesCmd := &cobra.Command{Use: "homes", Short: "List discovered and registered homes", Args: cobra.NoArgs}
	registerCmd := &cobra.Command{Use: "register <alias> <path>", Short: "Alias a home in ~/.config/vybava/memo/homes.json", Args: cobra.ArbitraryArgs}
	registerCmd.RunE = func(cmd *cobra.Command, args []string) error {
		s := session(cmd)
		if len(args) != 2 {
			return finish(s, nil, nil, nil, usage("register takes <alias> <path>", "memo homes register fixit-team /path/to/repo/.claude/memory"))
		}
		env := memoEnv()
		if d, err := env.Register(args[0], args[1]); d != nil || err != nil {
			return finish(s, nil, nil, nil, diagOrErr(d, err))
		}
		return finish(s, map[string]any{"alias": args[0], "path": args[1], "registry": env.RegistryPath()}, []string{"memo homes --json"}, nil, nil)
	}
	aliasCmd := &cobra.Command{Use: "alias <path> <alias>", Short: "Rewrite the alias in a ledger's frontmatter", Args: cobra.ArbitraryArgs}
	aliasCmd.RunE = func(cmd *cobra.Command, args []string) error {
		s := session(cmd)
		if len(args) != 2 {
			return finish(s, nil, nil, nil, usage("alias takes <path> <alias>", "memo homes alias ~/.claude/projects/<slug>/memory fixit"))
		}
		env := memoEnv()
		homes, d, err := env.Resolve(args[0], "")
		if d != nil || err != nil {
			return finish(s, nil, nil, nil, diagOrErr(d, err))
		}
		ledger := filepath.Join(homes[0].Path, memo.LedgerFile)
		if d, err := memo.SetAlias(ledger, args[1]); d != nil || err != nil {
			return finish(s, nil, nil, nil, diagOrErr(d, err))
		}
		return finish(s, map[string]any{"home": homes[0].Path, "alias": args[1]}, []string{"memo homes --json", "memo render --home " + homes[0].Path + " --json"}, nil, nil)
	}
	homesCmd.AddCommand(registerCmd, aliasCmd)
	homesCmd.RunE = func(cmd *cobra.Command, _ []string) error {
		s := session(cmd)
		env := memoEnv()
		homes, d, err := env.Discover()
		if d != nil || err != nil {
			return finish(s, nil, nil, nil, diagOrErr(d, err))
		}
		if !rt.json {
			for _, h := range homes {
				fmt.Fprintf(rt.stdout, "%-16s %-9s %-11s %s\n", h.Alias, h.Kind, h.Source, h.Path)
			}
			return finish(s, nil, []string{"memo vault --json"}, nil, nil)
		}
		return finish(s, map[string]any{"homes": homes, "registry": env.RegistryPath()}, []string{"memo vault --json"}, nil, nil)
	}

	// vault
	var vaultPath string
	vault := &cobra.Command{Use: "vault", Short: "Maintain the Obsidian vault of home symlinks", Args: cobra.NoArgs}
	vault.RunE = func(cmd *cobra.Command, _ []string) error {
		s := session(cmd)
		env := memoEnv()
		homes, d, err := env.Discover()
		if d != nil || err != nil {
			return finish(s, nil, nil, nil, diagOrErr(d, err))
		}
		path := vaultPath
		if path == "" {
			path = filepath.Join(env.UserHome, "Memory")
		}
		report, err := memo.Vault(path, homes)
		if err != nil {
			return finish(s, nil, nil, nil, err)
		}
		return finish(s, report, []string{"open " + path}, nil, nil)
	}
	vault.Flags().StringVar(&vaultPath, "path", "", "vault directory (default ~/Memory)")

	// snapshot / log / restore
	oneHome := func() (memo.Home, error) {
		env := memoEnv()
		homes, d, err := env.Resolve(homeSpec, "")
		if d != nil || err != nil {
			return memo.Home{}, diagOrErr(d, err)
		}
		for _, h := range homes {
			if h.Kind == memo.KindPersonal {
				return h, nil
			}
		}
		return homes[0], nil
	}
	var message string
	snapshot := &cobra.Command{Use: "snapshot", Short: "Commit the personal home into its local git history", Args: cobra.NoArgs}
	snapshot.RunE = func(cmd *cobra.Command, _ []string) error {
		s := session(cmd)
		h, err := oneHome()
		if err != nil {
			return finish(s, nil, nil, nil, err)
		}
		rev, d, err := memo.Snapshot(h, message)
		if d != nil || err != nil {
			return finish(s, map[string]any{"home": h.Path}, []string{"memo log" + homeArg(h) + " --json"}, nil, diagOrErr(d, err))
		}
		return finish(s, map[string]any{"home": h.Path, "rev": rev}, []string{"memo log" + homeArg(h) + " --json"}, nil, nil)
	}
	snapshot.Flags().StringVarP(&message, "message", "m", "", "commit message")
	var count int
	logCmd := &cobra.Command{Use: "log", Short: "List snapshots of the personal home", Args: cobra.NoArgs}
	logCmd.RunE = func(cmd *cobra.Command, _ []string) error {
		s := session(cmd)
		h, err := oneHome()
		if err != nil {
			return finish(s, nil, nil, nil, err)
		}
		entries, d, err := memo.Log(h, count)
		if d != nil || err != nil {
			return finish(s, nil, nil, nil, diagOrErr(d, err))
		}
		if !rt.json {
			for _, e := range entries {
				fmt.Fprintf(rt.stdout, "%s  %s  %s\n", e.Rev, e.At, e.Message)
			}
			return finish(s, nil, []string{"memo restore <rev> LEDGER.md" + homeArg(h)}, nil, nil)
		}
		return finish(s, map[string]any{"home": h.Path, "snapshots": entries}, []string{"memo restore <rev> LEDGER.md" + homeArg(h)}, nil, nil)
	}
	logCmd.Flags().IntVarP(&count, "count", "n", 20, "number of snapshots")
	restore := &cobra.Command{Use: "restore <rev> <file>", Short: "Bring one file back from a snapshot", Args: cobra.ArbitraryArgs}
	restore.RunE = func(cmd *cobra.Command, args []string) error {
		s := session(cmd)
		if len(args) != 2 {
			return finish(s, nil, nil, nil, usage("restore takes <rev> <file>", "memo restore <rev> LEDGER.md"))
		}
		h, err := oneHome()
		if err != nil {
			return finish(s, nil, nil, nil, err)
		}
		d, err := memo.Restore(h, args[0], args[1])
		if d != nil || err != nil {
			return finish(s, nil, nil, nil, diagOrErr(d, err))
		}
		return finish(s, map[string]any{"home": h.Path, "rev": args[0], "file": args[1]}, []string{"memo render" + homeArg(h) + " --json", "memo snapshot" + homeArg(h) + " -m \"restore " + args[1] + " from " + args[0] + "\""}, nil, nil)
	}

	// hook
	hook := &cobra.Command{Use: "hook", Short: "Claude Code / Codex hook: guard ledger files, harvest citations, render at SessionStart", Args: cobra.NoArgs}
	hook.RunE = func(cmd *cobra.Command, _ []string) error {
		payload, err := memo.ReadHookPayload(rt.stdin)
		if err != nil {
			return nil // fail open on an unreadable payload, like memorylint
		}
		res, err := memoEnv().RunHook(payload, time.Now())
		if err != nil {
			fmt.Fprintln(rt.stderr, "memo hook:", err)
			return nil
		}
		if res.Skipped != "" {
			fmt.Fprintln(rt.stderr, "memo hook: no home credited:", res.Skipped)
		}
		for _, problem := range res.Problems {
			fmt.Fprintln(rt.stderr, "memo hook: not rendered:", problem)
		}
		if res.Refused != nil {
			fmt.Fprintf(rt.stderr, "memo: %s\n  %s\n", res.Refused.Detail, res.Refused.Fix)
			return runx.ExitError{Code: 2}
		}
		if rt.json {
			return finish(session(cmd), res, []string{}, nil, nil)
		}
		return nil
	}

	root.AddCommand(add, show, find, touch, render, ensure, imp, migrate, homesCmd, vault, snapshot, logCmd, restore, hook)
	return root
}

func diagOrErr(d *memo.Diag, err error) error {
	if d != nil {
		return d
	}
	return err
}

func flagIf(home string) string {
	if home == "" {
		return ""
	}
	return " --home " + home
}

func matchesAll(r memo.Row, words []string) bool {
	hay := strings.ToLower(r.Sentence + " " + r.Topic + " " + r.Type)
	for _, w := range words {
		if !strings.Contains(hay, strings.ToLower(w)) {
			return false
		}
	}
	return true
}

// recordAdded stamps one `add` event per new row (all at now, so an import
// shares one creation time) and returns the home's full event history for
// the render that follows. The row itself carries no date.
func recordAdded(env memo.Env, home string, rows []memo.Row, now time.Time) ([]memo.Event, error) {
	existing, d, err := memo.LoadEvents(home)
	if d != nil {
		return nil, d
	}
	if err != nil {
		return nil, err
	}
	incoming := make([]memo.Event, 0, len(rows))
	for _, r := range rows {
		incoming = append(incoming, memo.NewEvent(r.ID, "add", env.Session, now))
	}
	if _, err := memo.AppendEvents(home, existing, incoming); err != nil {
		return nil, err
	}
	events, d, err := memo.LoadEvents(home)
	if d != nil {
		return nil, d
	}
	return events, err
}

// recordEvent appends one usage event for the session and re-renders.
func recordEvent(env memo.Env, home string, row int, kind string) error {
	existing, d, err := memo.LoadEvents(home)
	if d != nil {
		return d
	}
	if err != nil {
		return err
	}
	now := time.Now()
	if _, err := memo.AppendEvents(home, existing, []memo.Event{memo.NewEvent(row, kind, env.Session, now)}); err != nil {
		return err
	}
	l, d, err := memo.Load(filepath.Join(home, memo.LedgerFile))
	if d != nil {
		return d
	}
	if err != nil {
		return err
	}
	all, d, err := memo.LoadEvents(home)
	if d != nil {
		return d
	}
	if err != nil {
		return err
	}
	// The event is what show/touch owe; a tracked surface stays as committed
	// and `memo render` names its fix.
	_, _, err = memo.WriteIndex(l, all, now)
	return err
}

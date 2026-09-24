package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/henderson-tech/vybava/internal/lok"
	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/spf13/cobra"
)

func (rt *runtime) lokApplet() *cobra.Command {
	cmd := rt.lokCommand("lok")
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	cmd.SetOut(rt.stdout)
	cmd.SetErr(rt.stderr)
	cmd.PersistentFlags().BoolVar(&rt.json, "json", false, "emit machine-readable output")
	return cmd
}

// lokCommand wires the locale-catalog verbs. Every verb: one envelope, the
// closed lok.Diag codes, `next` naming the follow-up command.
func (rt *runtime) lokCommand(use string) *cobra.Command {
	root := &cobra.Command{
		Use:   use,
		Short: "Locale catalogs for AI sessions — query by key, write by verb, every locale kept in sync",
		Long: "lok never shows a whole catalog. Configure catalogs in the `lok` section of\n" +
			"vybava.config.ts (see `vybava config init`), then:\n" +
			"  lok catalogs · lok get <key> · lok grep <pattern> · lok missing · lok check\n" +
			"  lok add <key> --tr cs=… · lok set <key> --tr cs=… · lok rm <key> · lok scan [--write]\n" +
			"  lok merge <path> [--prefer ours|theirs] — settle a catalog merge conflict by key",
	}
	var catalog string
	root.PersistentFlags().StringVar(&catalog, "catalog", "", "catalog id (only when inference is ambiguous; use --catalog=<id>)")

	session := func(cmd *cobra.Command) *runx.Session {
		return &runx.Session{Tool: "lok", JSON: rt.json, Verb: cmd.Name(), Stdout: rt.stdout, Stderr: rt.stderr}
	}
	finish := func(s *runx.Session, data any, next []string, err error) error {
		env := runx.Envelope{OK: err == nil, Verb: s.Verb, Data: data, Diagnostics: []runx.Diagnostic{}, Next: next}
		if env.Next == nil {
			env.Next = []string{}
		}
		var d *lok.Diag
		if errors.As(err, &d) {
			env.Diagnostics = append(env.Diagnostics, runx.Diagnostic{Code: d.Code, Severity: "error", Detail: d.Detail, Fix: d.Fix})
			if d.Fix != "" {
				env.Next = append(env.Next, d.Fix)
			}
			err = &runx.DiagError{Diag: env.Diagnostics[0]}
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
	open := func() (*lok.Tool, error) { return lok.Open(workingDir()) }
	parseTr := func(tr []string) (map[string]string, error) {
		out := map[string]string{}
		for _, kv := range tr {
			loc, val, ok := strings.Cut(kv, "=")
			if !ok || loc == "" {
				return nil, &lok.Diag{Code: lok.DiagLocaleUnknown, Detail: fmt.Sprintf("--tr %q is not <locale>=<value>", kv), Fix: "--tr cs='Uložit'"}
			}
			out[loc] = val
		}
		return out, nil
	}

	root.AddCommand(&cobra.Command{
		Use: "catalogs", Short: "List configured catalogs with key counts and gaps per locale", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := session(cmd)
			t, err := open()
			if err != nil {
				return finish(s, nil, nil, err)
			}
			info, err := t.Catalogs()
			return finish(s, info, []string{"lok missing --json", "lok check --json"}, err)
		},
	})
	root.AddCommand(&cobra.Command{
		Use: "get <key>", Short: "One key across every locale", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s := session(cmd)
			t, err := open()
			if err != nil {
				return finish(s, nil, nil, err)
			}
			v, err := t.Get(catalog, args[0])
			var next []string
			if err == nil && len(v.Missing) > 0 {
				next = []string{"lok set " + quoteArg(args[0]) + " --tr " + v.Missing[0] + "=<value>"}
			}
			return finish(s, v, next, err)
		},
	})
	var locales []string
	var limit int
	grep := &cobra.Command{
		Use: "grep <pattern>", Short: "Regex over keys and values (case-insensitive), capped", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s := session(cmd)
			t, err := open()
			if err != nil {
				return finish(s, nil, nil, err)
			}
			res, err := t.Grep(catalog, args[0], locales, limit)
			var next []string
			if res.Truncated {
				next = []string{fmt.Sprintf("lok grep %s --limit %d  # or narrow the pattern", quoteArg(args[0]), res.Total)}
			}
			return finish(s, res, next, err)
		},
	}
	grep.Flags().StringSliceVar(&locales, "locale", nil, "only these locales")
	grep.Flags().IntVar(&limit, "limit", 20, "max hits returned")
	root.AddCommand(grep)

	var tr []string
	add := &cobra.Command{
		Use: "add <key>", Short: "Insert a new key into every locale (en derives from the key; required locales need --tr)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s := session(cmd)
			t, err := open()
			if err != nil {
				return finish(s, nil, nil, err)
			}
			m, err := parseTr(tr)
			if err != nil {
				return finish(s, nil, nil, err)
			}
			res, err := t.Add(catalog, args[0], m)
			return finish(s, res, []string{"lok get " + quoteArg(args[0]) + " --json"}, err)
		},
	}
	add.Flags().StringArrayVar(&tr, "tr", nil, "<locale>=<value>, repeatable")
	root.AddCommand(add)

	var trSet []string
	set := &cobra.Command{
		Use: "set <key>", Short: "Update an existing key in the given locales", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s := session(cmd)
			t, err := open()
			if err != nil {
				return finish(s, nil, nil, err)
			}
			m, err := parseTr(trSet)
			if err != nil {
				return finish(s, nil, nil, err)
			}
			res, err := t.Set(catalog, args[0], m)
			return finish(s, res, []string{"lok get " + quoteArg(args[0]) + " --json"}, err)
		},
	}
	set.Flags().StringArrayVar(&trSet, "tr", nil, "<locale>=<value>, repeatable")
	root.AddCommand(set)

	root.AddCommand(&cobra.Command{
		Use: "rm <key>", Short: "Delete a key (and its plural variants) from every locale", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s := session(cmd)
			t, err := open()
			if err != nil {
				return finish(s, nil, nil, err)
			}
			res, err := t.Rm(catalog, args[0])
			return finish(s, res, []string{"lok check --json"}, err)
		},
	})

	var missLocales []string
	var missLimit int
	var all bool
	missing := &cobra.Command{
		Use: "missing", Short: "Keys a required locale lacks (--all: every locale)", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := session(cmd)
			t, err := open()
			if err != nil {
				return finish(s, nil, nil, err)
			}
			gaps, total, err := t.Missing(catalog, missLocales, !all, missLimit)
			var next []string
			if total > 0 {
				next = []string{"lok set " + quoteArg(gaps[0].Key) + " --catalog=" + gaps[0].Catalog + " --tr " + gaps[0].Locale + "=<value>"}
			}
			return finish(s, map[string]any{"gaps": gaps, "total": total, "truncated": total > len(gaps)}, next, err)
		},
	}
	missing.Flags().StringSliceVar(&missLocales, "locale", nil, "only these locales")
	missing.Flags().IntVar(&missLimit, "limit", 50, "max gaps returned")
	missing.Flags().BoolVar(&all, "all", false, "report every locale, not only required ones")
	root.AddCommand(missing)

	root.AddCommand(&cobra.Command{
		Use: "check", Short: "Parity, english-as-key and placeholder invariants — CI gate", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := session(cmd)
			t, err := open()
			if err != nil {
				return finish(s, nil, nil, err)
			}
			problems, err := t.Check(catalog)
			if err == nil {
				// Warnings (severity "warning") ride along in the output but never fail the gate.
				var errs []lok.Problem
				for _, p := range problems {
					if p.Severity == "" {
						errs = append(errs, p)
					}
				}
				if len(errs) > 0 {
					err = &lok.Diag{Code: lok.DiagCheckFailed, Detail: fmt.Sprintf("%d problem(s); first: %s %q [%s] %s", len(errs), errs[0].Kind, errs[0].Key, errs[0].Locale, errs[0].Detail), Fix: "lok missing --json"}
				}
			}
			if problems == nil {
				problems = []lok.Problem{}
			}
			return finish(s, map[string]any{"problems": problems}, nil, err)
		},
	})

	var write bool
	var orphanLimit int
	scan := &cobra.Command{
		Use: "scan", Short: "Extract literal t('…') keys from source: add the missing (--write), list probable orphans", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s := session(cmd)
			t, err := open()
			if err != nil {
				return finish(s, nil, nil, err)
			}
			res, err := t.Scan(catalog, write, orphanLimit)
			var next []string
			if err == nil && len(res.Missing) > 0 && !write {
				next = append(next, "lok scan --write --json")
			}
			if err == nil && len(res.Added) > 0 {
				next = append(next, "lok missing --json  # translate what scan added")
			}
			return finish(s, res, next, err)
		},
	}
	scan.Flags().BoolVar(&write, "write", false, "add the missing keys (en = key); translations then show in `lok missing`")
	scan.Flags().IntVar(&orphanLimit, "orphans", 20, "max probable orphans listed")
	root.AddCommand(scan)

	root.AddCommand(&cobra.Command{
		Use: "merge-driver <base> <ours> <theirs> <path>", Short: "Git merge driver: 3-way merge of a declared catalog by key (registered by `merge-assist setup`)", Args: cobra.ExactArgs(4),
		RunE: func(_ *cobra.Command, args []string) error {
			conflict, err := lok.MergeDriver(workingDir(), args[0], args[1], args[2], args[3], rt.stderr)
			if err != nil {
				return err
			}
			if conflict {
				return runx.ExitError{Code: 1}
			}
			return nil
		},
	})

	var prefer string
	merge := &cobra.Command{
		Use: "merge <path>", Short: "Re-merge an unmerged catalog from the index by key, write and stage it (--prefer settles clashes)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s := session(cmd)
			if prefer != "" && prefer != string(lok.PreferOurs) && prefer != string(lok.PreferTheirs) {
				return finish(s, nil, nil, &lok.Diag{Code: lok.DiagMergeClash, Detail: fmt.Sprintf("--prefer %q", prefer), Fix: "--prefer ours or --prefer theirs"})
			}
			t, err := open()
			if err != nil {
				return finish(s, nil, nil, err)
			}
			res, err := t.MergeIndexed(args[0], lok.Prefer(prefer))
			var next []string
			if err == nil && res.Regen != "" {
				next = []string{"merge-assist regen"}
			}
			return finish(s, res, next, err)
		},
	}
	merge.Flags().StringVar(&prefer, "prefer", "", "settle clashing keys toward ours or theirs")
	root.AddCommand(merge)
	return root
}

func quoteArg(s string) string {
	if !strings.ContainsAny(s, " '\"$`\\{};&|<>()!#*?[]~") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// workingDir is the directory the verb resolves the repo config from.
func workingDir() string {
	d, err := os.Getwd()
	if err != nil {
		return "."
	}
	return d
}

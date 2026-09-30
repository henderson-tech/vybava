package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/henderson-tech/vybava/internal/lok"
	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/henderson-tech/vybava/internal/shellword"
	"github.com/spf13/cobra"
)

// lokRewrite wires the two rewrite verbs, `lok sub` and `lok mv`, onto the
// shared lok root: its --catalog flag, session and envelope.
type lokRewrite struct {
	catalog *string
	session func(*cobra.Command) *runx.Session
	open    func() (*lok.Tool, error)
	finish  func(*runx.Session, lok.SubResult, []string, error) error
}

// rewriteFlags are the flags sub and mv share; each verb registers its own.
type rewriteFlags struct {
	locales       []string
	key, exclude  string
	literal       bool
	ignoreCase    bool
	write         bool
	expect        int
	limit         int
	refs          bool
	keys          bool
	merge         bool
	withMirrors   bool
	noSource      bool
	allowLiterals bool
}

func (f *rewriteFlags) options(catalog string) lok.SubOptions {
	o := lok.SubOptions{Locales: f.locales, Key: f.key, ExcludeKey: f.exclude, Literal: f.literal, IgnoreCase: f.ignoreCase,
		Write: f.write, Expect: f.expect, Limit: f.limit, Refs: f.refs, Keys: f.keys, Merge: f.merge,
		WithMirrors: f.withMirrors, NoSource: f.noSource, AllowLiterals: f.allowLiterals}
	for _, id := range strings.Split(catalog, ",") {
		if id = strings.TrimSpace(id); id != "" {
			o.Catalogs = append(o.Catalogs, id)
		}
	}
	return o
}

// command re-renders the invocation (scope flags included, run flags not),
// so `next` is the exact command the agent reviewed.
func (f *rewriteFlags) command(verb string, args []string, catalog string) []string {
	parts := []string{"lok", verb}
	for _, a := range args {
		parts = append(parts, shellword.Quote(a))
	}
	if catalog != "" {
		parts = append(parts, "--catalog="+catalog)
	}
	if len(f.locales) > 0 {
		parts = append(parts, "--locale", strings.Join(f.locales, ","))
	}
	if f.key != "" {
		parts = append(parts, "--key", shellword.Quote(f.key))
	}
	if f.exclude != "" {
		parts = append(parts, "--exclude-key", shellword.Quote(f.exclude))
	}
	for _, b := range []struct {
		on   bool
		flag string
	}{{f.literal && verb == "sub", "-F"}, {f.ignoreCase, "-i"}, {f.keys && verb == "sub", "--keys"}, {f.merge, "--merge"}, {f.withMirrors, "--with-mirrors"}, {f.noSource, "--no-source"}, {f.allowLiterals, "--allow-literals"}} {
		if b.on {
			parts = append(parts, b.flag)
		}
	}
	return parts
}

// next: after a dry run, the exact write command (with the reviewed count
// for sub); after a write, the checks that prove it; a second pass when
// rewritten values still match.
func (f *rewriteFlags) next(verb string, args []string, catalog string, res lok.SubResult, err error, t *lok.Tool) []string {
	var d *lok.Diag
	if errors.As(err, &d) {
		// A refusal one flag settles: its Fix becomes that exact command (still a dry run).
		cmd := strings.Join(f.command(verb, args, catalog), " ")
		switch {
		case d.Code == lok.DiagMirrorSource:
			d.Fix = cmd + " --with-mirrors"
		case d.Code == lok.DiagKeyExists && d.Fix == lok.FixRerunWithMerge:
			d.Fix = cmd + " --merge"
		}
	}
	if err != nil {
		return nil
	}
	cmd := strings.Join(f.command(verb, args, catalog), " ")
	count := res.Total.Values
	if res.Mode == "keys" {
		count = res.Total.Renames
	}
	var out []string
	if !res.Write {
		if count == 0 {
			return nil
		}
		// Leftover literals make that write refuse (CALL_SITES_UNRESOLVED),
		// so it is no next step; the warning names how to resolve them.
		switch {
		case res.Total.Literals > 0 && !f.allowLiterals:
		case verb == "sub":
			out = append(out, fmt.Sprintf("%s --write --expect %d --json", cmd, count))
		default:
			out = append(out, cmd+" --write --json")
		}
		if res.Truncated {
			all := max(res.Total.Values, res.Total.CallSites, res.Total.Literals, res.Total.Mirrors)
			out = append(out, fmt.Sprintf("%s --limit %d  # list every change first", cmd, all))
		}
		return out
	}
	for _, bc := range res.ByCatalog {
		if len(bc.Written) == 0 {
			continue
		}
		out = append(out, "lok check --catalog="+bc.Catalog+" --json")
		if cfg, ok := t.Config.Catalogs[bc.Catalog]; ok && res.Mode == "keys" && cfg.Scan != nil && !f.noSource {
			out = append(out, "lok scan --catalog="+bc.Catalog+" --json")
		}
	}
	if res.StillMatching > 0 {
		out = append(out, fmt.Sprintf("%s --json  # %d rewritten value(s) match again: review a second pass", cmd, res.StillMatching))
	}
	return out
}

const subLong = `Rewrite catalog values with a Go RE2 regex (every non-overlapping match).
A dry run by default: review it, then run the printed
` + "`--write --expect <n>`" + ` command, which refuses (SUB_DRIFT) if the count moved.

Case-sensitive by default (-i to ignore case): a rewrite writes the
replacement literally, so (?i) would lowercase sentence-initial matches.
Templates: $1, ${1}, ${name}, $$. Write ${1}a, never $1a (Go reads the
group named "1a" and expands it to nothing: BAD_REPLACEMENT). Single-quote
the replacement or the shell eats $1. \x{HHHH} and \\ are decoded in the
replacement, so an invisible character is typed visibly: '\x{00A0}'.
RE2 has no lookaround or backreferences, and \b / \w are ASCII-only
(\bmáš\b never matches Czech text): use (^|\PL) ... (\PL|$) and re-emit ${1}.

Refused before anything is written: a changed {{x}} / {x} placeholder set,
an emptied value, a new ` + "`lok check`" + ` problem, a file changed since load.
english-as-key catalogs: en base values ARE the key and are skipped
(en-is-key); --keys renames keys instead, t() call sites included.`

func (r lokRewrite) subCommand() *cobra.Command {
	f := &rewriteFlags{}
	cmd := &cobra.Command{
		Use: "sub <pattern> <replacement>", Short: "Regex rewrite over catalog values (--keys: english-as-key key renames); a dry run unless --write",
		Long: subLong, Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s := r.session(cmd)
			t, err := r.open()
			if err != nil {
				return r.finish(s, lok.SubResult{}, nil, err)
			}
			o := f.options(*r.catalog)
			o.Pattern, o.Replacement = args[0], args[1]
			res, err := t.Sub(o)
			return r.finish(s, res, f.next("sub", args, *r.catalog, res, err, t), err)
		},
	}
	fl := cmd.Flags()
	fl.StringSliceVar(&f.locales, "locale", nil, "only these locales (values mode)")
	fl.StringVar(&f.key, "key", "", "only keys matching this RE2 (the canonical, escaped key)")
	fl.StringVar(&f.exclude, "exclude-key", "", "skip keys matching this RE2 (RE2 has no lookahead)")
	fl.BoolVarP(&f.literal, "literal", "F", false, "match the pattern literally; the replacement takes no $ templates")
	fl.BoolVarP(&f.ignoreCase, "ignore-case", "i", false, "case-insensitive match (the default is case-sensitive)")
	fl.BoolVar(&f.write, "write", false, "apply (without it: a dry run)")
	fl.IntVar(&f.expect, "expect", -1, "refuse unless exactly n values (--keys: key families) change")
	fl.IntVar(&f.limit, "limit", 20, "max changes listed (totals stay complete; 0 = counts only)")
	fl.BoolVar(&f.refs, "refs", false, "list test sources holding an old value verbatim")
	fl.BoolVar(&f.keys, "keys", false, "rename english-as-key keys (base keys) instead of rewriting values")
	r.renameFlags(cmd, f)
	return cmd
}

func (r lokRewrite) renameFlags(cmd *cobra.Command, f *rewriteFlags) {
	fl := cmd.Flags()
	fl.BoolVar(&f.merge, "merge", false, "the new key already exists with identical values: drop the old family, repoint its call sites")
	fl.BoolVar(&f.withMirrors, "with-mirrors", false, "also rewrite the key's source literal in the catalog's mirrors.roots")
	fl.BoolVar(&f.noSource, "no-source", false, "skip the call-site rewrite (a typecheck names every stale site)")
	fl.BoolVar(&f.allowLiterals, "allow-literals", false, "write even though quoted old keys remain outside t() calls")
}

func (r lokRewrite) mvCommand() *cobra.Command {
	f := &rewriteFlags{keys: true}
	cmd := &cobra.Command{
		Use:   "mv <old-key> <new-key>",
		Short: "Rename one english-as-key family (plural variants per locale) and its t() call sites; a dry run unless --write",
		Long: `Rename one english-as-key key family: the base key and every plural variant
each locale holds move to the new key at its sorted slot (like lok add); en
values that equal their key follow it. Literal t('old') call sites under the
catalog's scan roots are rewritten (tests included and reported); any other
quoted 'old' left there refuses --write (CALL_SITES_UNRESOLVED) unless
--allow-literals. A catalog with mirrors (API error sentences) needs
--with-mirrors to rewrite the source literal too. \x{HHHH} is decoded in both
keys. Without --catalog every english-as-key catalog holding the key moves.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s := r.session(cmd)
			t, err := r.open()
			if err != nil {
				return r.finish(s, lok.SubResult{}, nil, err)
			}
			o := f.options(*r.catalog)
			res, err := t.Mv(*r.catalog, args[0], args[1], o)
			return r.finish(s, res, f.next("mv", args, *r.catalog, res, err, t), err)
		},
	}
	cmd.Flags().BoolVar(&f.write, "write", false, "apply (without it: a dry run)")
	cmd.Flags().IntVar(&f.limit, "limit", 20, "max changes listed")
	f.expect = -1
	r.renameFlags(cmd, f)
	return cmd
}

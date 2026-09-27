package readeff

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// RenderReport writes the human report: the headline numbers, then agents,
// repositories and the most-read files.
func RenderReport(w io.Writer, r Report) {
	scope := "all repositories"
	if r.Repo != "" {
		scope = short(r.Repo, "")
	}
	fmt.Fprintf(w, "%s · %d sessions since %s\n\n", scope, r.Total.Sessions, r.Since.Local().Format("2006-01-02"))
	renderSummary(w, r.Total, r.Config, "  ")
	if len(r.Agents) > 1 {
		fmt.Fprintln(w, "\n  by agent")
		renderScopes(w, r.Agents)
	}
	if r.Repo == "" && len(r.Repos) > 0 {
		fmt.Fprintln(w, "\n  by repository")
		renderScopes(w, r.Repos[:min(len(r.Repos), 10)])
	}
	if len(r.Files) > 0 {
		fmt.Fprintln(w, "\n  most-read files")
		renderFiles(w, r.Files, r.Repo)
	}
}

func renderSummary(w io.Writer, s Summary, cfg Config, indent string) {
	row := func(label, value, note string) {
		fmt.Fprintf(w, "%s%-26s %10s   %s\n", indent, label, value, note)
	}
	row("lines read / line changed", ratio(s.LinesPerChange), fmt.Sprintf("read %s · search %s · mixed %s · changed %s", num(s.ReadLines), num(s.SearchLines), num(s.MixedLines), num(s.Changed)))
	row("re-read rate", fmt.Sprintf("%.1f %%", s.RereadRate), fmt.Sprintf("%s of %s ranged reads · %s lines", num(s.Rereads), num(s.RangedReads), num(s.RereadLines)))
	row("whole-file big reads", num(s.BigWhole), fmt.Sprintf("files over %d lines · %s lines", cfg.BigFile, num(s.BigWholeLines)))
	row("search → read hit rate", fmt.Sprintf("%.1f %%", s.HitRate), fmt.Sprintf("%s of %s searches that printed files", num(s.SearchHits), num(s.SearchesFound)))
	row("empty searches", num(s.EmptySearches), fmt.Sprintf("%s re-run and found something", num(s.RetriedEmpty)))
	row("stale-state re-shows", num(s.Stale), fmt.Sprintf("%d compactions", s.Compactions))
	if len(s.Blocked) > 0 {
		row("guard blocks", num(sum(s.Blocked)), topRules(s.Blocked, 3))
	}
}

func renderScopes(w io.Writer, scopes []Scope) {
	for _, s := range scopes {
		fmt.Fprintf(w, "    %-48s %5d sessions  %9s nav lines  %6s /change  re-read %5.1f %%\n",
			short(s.Name, ""), s.Sessions, num(s.ReadLines+s.SearchLines+s.MixedLines), ratio(s.LinesPerChange), s.RereadRate)
	}
}

func renderFiles(w io.Writer, files []FileStat, repo string) {
	for _, f := range files {
		fmt.Fprintf(w, "    %-52s %9s lines  %3d sessions  %3d reads (%d unsized)  %3d re-reads\n", short(f.Path, repo), num(f.Lines), f.Sessions, f.Reads, f.Unsized, f.Rereads)
	}
}

// RenderFiles writes the file ranking alone.
func RenderFiles(w io.Writer, files []FileStat, repo string) {
	if len(files) == 0 {
		fmt.Fprintln(w, "no file reads in the window")
		return
	}
	renderFiles(w, files, repo)
}

// RenderTrace writes a session call by call.
func RenderTrace(w io.Writer, t SessionTrace, cfg Config) {
	fmt.Fprintf(w, "%s %s · %s · %s\n\n", t.Agent, t.ID, short(t.Repo, ""), t.Start.Local().Format("2006-01-02 15:04"))
	for _, st := range t.Steps {
		files := make([]string, 0, len(st.Files))
		for _, f := range st.Files {
			files = append(files, short(f, t.Repo))
		}
		fmt.Fprintf(w, "  %4d  %-14s %-6s %6d  %-40s %s\n", st.Index, trunc(st.Tool, 14), st.Class, st.Lines, trunc(strings.Join(files, " "), 40), strings.Join(st.Flags, " "))
	}
	fmt.Fprintln(w)
	renderSummary(w, t.Summary, cfg, "  ")
}

// short shows a path relative to base (or the home) when it lies inside it.
func short(p, base string) string {
	if base != "" {
		if rel, err := filepath.Rel(base, p); err == nil && !strings.HasPrefix(rel, "..") {
			return rel
		}
	}
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, home+"/") {
		return "~" + p[len(home):]
	}
	return p
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func ratio(f float64) string {
	if f == 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f", f)
}

// num groups thousands with commas.
func num(n int) string {
	s := fmt.Sprint(n)
	for i := len(s) - 3; i > 0 && s[i-1] != '-'; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func sum(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

func topRules(m map[string]int, n int) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, 0, n)
	for _, k := range keys[:min(n, len(keys))] {
		parts = append(parts, fmt.Sprintf("%s %d", k, m[k]))
	}
	return strings.Join(parts, " · ")
}

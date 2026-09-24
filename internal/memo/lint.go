package memo

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Finding is one lint result; memorylint converts it into its own shape.
type Finding struct {
	Rule     string `json:"rule"`
	Severity string `json:"severity"`
	Path     string `json:"path"`
	Line     int    `json:"line,omitempty"`
	Message  string `json:"message"`
}

// Lint rules for a ledger home. L001 grammar (incl. long dash, period, two
// sentences, length cap), L002 id order, L003 supersede/retire target,
// L004 link resolution, L005 length warning, L006 MEMORY.md drift, L007
// note not linked from any row, L008 a team home's hot surface (MEMORY.md,
// usage.jsonl) is committed instead of gitignored.
const (
	RuleGrammar          = "L001"
	RuleIDOrder          = "L002"
	RuleTarget           = "L003"
	RuleLink             = "L004"
	RuleLength           = "L005"
	RuleDrift            = "L006"
	RuleNoteOrphan       = "L007"
	RuleSurfaceCommitted = "L008"
)

// HasLedger reports whether a home is a memo ledger home.
func HasLedger(home string) bool { return hasLedger(home) }

// Lint checks a ledger home: grammar, ids, targets, links (notes/, rows and
// registered aliases), lengths, MEMORY.md drift and orphan notes. aliases
// lists the homes cross-home links may point at.
func Lint(home string, aliases map[string]Home, now time.Time) []Finding {
	path := filepath.Join(home, LedgerFile)
	l, d, err := Load(path)
	if err != nil {
		return []Finding{{Rule: RuleGrammar, Severity: "error", Path: path, Message: err.Error()}}
	}
	if d != nil {
		return []Finding{{Rule: RuleGrammar, Severity: "error", Path: path, Line: d.Line, Message: d.Detail}}
	}
	var out []Finding
	linked := map[string]bool{}
	for _, r := range l.Rows {
		out = append(out, lintRow(l, r, aliases, linked)...)
	}
	events, d, err := LoadEvents(home)
	switch {
	case err != nil:
		out = append(out, Finding{Rule: RuleGrammar, Severity: "error", Path: filepath.Join(home, UsageFile), Message: err.Error()})
	case d != nil:
		out = append(out, Finding{Rule: RuleGrammar, Severity: "error", Path: filepath.Join(home, UsageFile), Line: d.Line, Message: d.Detail})
	default:
		// A team home's MEMORY.md is a local projection (gitignored, rendered
		// at SessionStart), so a committed tree without one is clean; a
		// personal home always carries its render. A tracked one is never
		// rendered over, so drift there is L008's to report, not L006's.
		_, statErr := os.Stat(filepath.Join(home, IndexFile))
		if (l.Kind != KindTeam || !os.IsNotExist(statErr)) && TrackedIndex(home, l.Kind) == nil {
			if same, err := CheckIndex(l, events, now); err != nil || !same {
				out = append(out, Finding{Rule: RuleDrift, Severity: "error", Path: filepath.Join(home, IndexFile), Line: 1, Message: "MEMORY.md differs from `memo render` output; run `memo render --home " + home + "`"})
			}
		}
	}
	if l.Kind == KindTeam {
		for _, f := range LocalFiles {
			path := filepath.Join(home, f)
			if _, err := os.Stat(path); err != nil {
				continue
			}
			ignored, known := GitIgnored(path)
			if !known || ignored {
				continue
			}
			msg := "team hot surface is committed (" + f + " is not gitignored); add MEMORY.md and usage.jsonl to " + filepath.Join(home, GitignoreFile) + " (`memo render --home " + home + "` writes it)"
			if tracked, _ := GitTracked(path); tracked {
				msg = "team hot surface is tracked by git (" + f + "), and no ignore line untracks it; untrack it: git -C " + home + " rm --cached -q -- " + f
				if d := TrackedIndex(home, l.Kind); f == IndexFile && d != nil {
					msg = d.Detail
				}
			}
			out = append(out, Finding{Rule: RuleSurfaceCommitted, Severity: "warning", Path: path, Line: 1, Message: msg})
		}
	}
	notes, _ := filepath.Glob(filepath.Join(home, NotesDir, "*.md"))
	for _, n := range notes {
		slug := strings.TrimSuffix(filepath.Base(n), ".md")
		if !linked[slug] {
			out = append(out, Finding{Rule: RuleNoteOrphan, Severity: "warning", Path: n, Line: 1, Message: "note is not linked from any ledger row; add `-> [[notes/" + slug + "]]` to the row it details"})
		}
	}
	return out
}

func lintRow(l *Ledger, r Row, aliases map[string]Home, linked map[string]bool) []Finding {
	var out []Finding
	at := func(rule, severity, msg string) {
		out = append(out, Finding{Rule: rule, Severity: severity, Path: l.Path, Line: r.Line, Message: fmt.Sprintf("row #%d: %s", r.ID, msg)})
	}
	if TypeKind[r.Type] != l.Kind {
		at(RuleGrammar, "error", fmt.Sprintf("type %s does not belong in a %s home", r.Type, l.Kind))
	}
	if d := SentenceWarning(r.Sentence); d != nil {
		at(RuleLength, "warning", d.Detail)
	}
	for _, target := range []int{r.Supersedes(), r.Retires()} {
		if target == 0 {
			continue
		}
		if target >= r.ID {
			at(RuleTarget, "error", fmt.Sprintf("target #%d is not an earlier row", target))
			continue
		}
		if _, ok := l.Find(target); !ok {
			at(RuleTarget, "error", fmt.Sprintf("target #%d does not exist", target))
			continue
		}
		if closer := firstCloser(l, target); closer != r.ID {
			at(RuleTarget, "error", fmt.Sprintf("target #%d was already closed by #%d", target, closer))
		}
	}
	for _, link := range r.Links {
		if msg := unresolvedLink(l, link, aliases, linked); msg != "" {
			at(RuleLink, "error", msg)
		}
	}
	return out
}

func firstCloser(l *Ledger, target int) int {
	for _, r := range l.Rows {
		if r.Supersedes() == target || r.Retires() == target {
			return r.ID
		}
	}
	return 0
}

func unresolvedLink(l *Ledger, link string, aliases map[string]Home, linked map[string]bool) string {
	switch {
	case strings.HasPrefix(link, "http"):
		return ""
	}
	if m := noteLinkRE.FindStringSubmatch(link); m != nil {
		linked[m[1]] = true
		if _, err := os.Stat(filepath.Join(l.Home(), NotesDir, m[1]+".md")); err != nil {
			return "link " + link + " points at a missing note"
		}
		return ""
	}
	if m := rowLinkRE.FindStringSubmatch(link); m != nil {
		id, _ := strconv.Atoi(m[1])
		if _, ok := l.Find(id); !ok {
			return "link " + link + " points at a missing row"
		}
		return ""
	}
	alias, rest := "", ""
	if m := aliasRowLinkRE.FindStringSubmatch(link); m != nil {
		alias, rest = m[1], "row #"+m[2]
	} else if m := aliasNoteLinkRE.FindStringSubmatch(link); m != nil {
		alias, rest = m[1], filepath.Join(NotesDir, m[2]+".md")
	}
	other, ok := aliases[alias]
	if !ok {
		return "link " + link + " names an alias no home carries; `memo homes --json`"
	}
	if strings.HasPrefix(rest, "row ") {
		ol, d, err := Load(filepath.Join(other.Path, LedgerFile))
		if err != nil || d != nil {
			return "link " + link + ": ledger of " + alias + " could not be read"
		}
		id, _ := strconv.Atoi(strings.TrimPrefix(rest, "row #"))
		if _, ok := ol.Find(id); !ok {
			return "link " + link + " points at a missing row in " + alias
		}
		return ""
	}
	if _, err := os.Stat(filepath.Join(other.Path, rest)); err != nil {
		return "link " + link + " points at a missing note in " + alias
	}
	return ""
}

// AliasMap indexes homes by alias for link resolution.
func AliasMap(homes []Home) map[string]Home {
	out := map[string]Home{}
	for _, h := range homes {
		out[h.Alias] = h
	}
	return out
}

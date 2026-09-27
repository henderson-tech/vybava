package readeff

import (
	"sort"
	"time"
)

// Summary is Totals with the derived rates a reader wants first.
type Summary struct {
	Totals
	LinesPerChange float64 `json:"lines_per_change"`
	RereadRate     float64 `json:"reread_pct"`
	HitRate        float64 `json:"search_hit_pct"`
}

func summarize(t Totals) Summary {
	return Summary{Totals: t, LinesPerChange: t.LinesPerChange(), RereadRate: t.RereadRate(), HitRate: t.HitRate()}
}

// Scope is a named slice of the report: one agent or one repository.
type Scope struct {
	Name string `json:"name"`
	Summary
}

// Report is one scan rolled up.
type Report struct {
	Since  time.Time  `json:"since"`
	Repo   string     `json:"repo,omitempty"`
	Config Config     `json:"config"`
	Total  Summary    `json:"total"`
	Agents []Scope    `json:"agents"`
	Repos  []Scope    `json:"repos"`
	Files  []FileStat `json:"files"`
}

// Builder folds sessions one at a time — overall, per agent, per repository
// and per file — so a scan never holds more than the session in hand.
type Builder struct {
	cfg           Config
	total         Totals
	agents, repos map[string]*Totals
	files         map[string]*fileAcc
}

// NewBuilder starts an empty rollup.
func NewBuilder(cfg Config) *Builder {
	return &Builder{cfg: cfg, agents: map[string]*Totals{}, repos: map[string]*Totals{}, files: map[string]*fileAcc{}}
}

// Add folds one session in; it fits Scan's callback.
func (b *Builder) Add(s Session) {
	t := analyze(s, b.cfg, b.files, nil)
	b.total.add(t)
	scopeOf(b.agents, s.Agent).add(t)
	scopeOf(b.repos, s.Repo).add(t)
}

// Report is the rollup so far; topFilesN bounds the file list.
func (b *Builder) Report(topFilesN int) Report {
	return Report{Config: b.cfg, Total: summarize(b.total), Agents: scopes(b.agents), Repos: scopes(b.repos), Files: topFiles(b.files, topFilesN)}
}

// Files ranks every file read so far; n bounds the list.
func (b *Builder) Files(n int) []FileStat { return topFiles(b.files, n) }

// SessionTrace is one session walked call by call.
type SessionTrace struct {
	Agent   string    `json:"agent"`
	ID      string    `json:"id"`
	Repo    string    `json:"repo"`
	Start   time.Time `json:"start"`
	Summary Summary   `json:"summary"`
	Steps   []Step    `json:"steps"`
}

// Trace walks one session and says what each call counted as.
func Trace(s Session, cfg Config) SessionTrace {
	steps := []Step{}
	t := analyze(s, cfg, map[string]*fileAcc{}, &steps)
	return SessionTrace{Agent: s.Agent, ID: s.ID, Repo: s.Repo, Start: s.Start, Summary: summarize(t), Steps: steps}
}

func scopeOf(m map[string]*Totals, name string) *Totals {
	if name == "" {
		name = "(no repository)"
	}
	if m[name] == nil {
		m[name] = &Totals{}
	}
	return m[name]
}

// scopes lists named totals, heaviest navigation first.
func scopes(m map[string]*Totals) []Scope {
	out := make([]Scope, 0, len(m))
	for name, t := range m {
		out = append(out, Scope{Name: name, Summary: summarize(*t)})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i].ReadLines+out[i].SearchLines, out[j].ReadLines+out[j].SearchLines
		if a != b {
			return a > b
		}
		return out[i].Name < out[j].Name
	})
	return out
}

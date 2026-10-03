package fleet

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/henderson-tech/vybava/internal/runx"
)

// ReviveSource says what proved the session dead.
type ReviveSource string

const (
	// ReviveRegistry: the registry still lists the session, its process is gone.
	ReviveRegistry ReviveSource = "registry"
	// ReviveLedger: the registry record is gone (Claude Code removes it on an
	// orderly exit, including a terminal closing under it) and the ledger's
	// owner process is proven gone with jobs still open.
	ReviveLedger ReviveSource = "ledger"
)

// ReviveSession is one session that died with work in flight.
type ReviveSession struct {
	SessionID string       `json:"sessionId"`
	Name      string       `json:"name,omitempty"`
	Project   string       `json:"project"`
	Root      string       `json:"root,omitempty"`
	Worktree  string       `json:"worktree,omitempty"`
	CWD       string       `json:"cwd,omitempty"`
	Status    string       `json:"status,omitempty"`
	Since     time.Time    `json:"since"`
	Source    ReviveSource `json:"source"`
	Liveness  Liveness     `json:"liveness"`
	Resume    string       `json:"resume"`
	OpenJobs  []Job        `json:"openJobs"`
}

// Revive lists the sessions to bring back, most recent death first.
type Revive struct {
	GeneratedAt time.Time       `json:"generatedAt"`
	Sessions    []ReviveSession `json:"sessions"`
}

// ReadRevive finds every session that died while busy or waiting, or with
// background jobs still open. Only proven deaths are listed: a session whose
// liveness is unknown, or a ledger with no owner stamp, is never offered.
func ReadRevive(ctx context.Context, env Env) (Revive, []runx.Diagnostic, error) {
	out := Revive{GeneratedAt: env.Now, Sessions: []ReviveSession{}}
	records, diags, err := readRegistry(env.registryDir())
	if err != nil {
		return out, diags, err
	}
	view, viewDiag := env.processView(ctx)
	diags = append(diags, viewDiag...)
	roots := resolver{home: env.Home, cache: map[string][2]string{}}
	dir := env.ledgerDir()

	registered := map[string]bool{}
	for _, rec := range records {
		registered[rec.SessionID] = true
		liveness := view.classify(rec.PID, rec.ProcStart, rec.PIDDomain, env.goos(), env.Now.Location())
		if !liveness.Dead() {
			continue
		}
		ledger, err := loadLedger(dir, rec.SessionID)
		if err != nil {
			diags = append(diags, warning(DiagLedgerUnreadable, err.Error()))
		}
		session := roots.session(rec, liveness, env.Now)
		if session.State != StateDead && ledger.open() == 0 {
			continue
		}
		out.Sessions = append(out.Sessions, ReviveSession{
			SessionID: rec.SessionID, Name: rec.Name, Project: session.Project, Root: session.Root, Worktree: session.Worktree,
			CWD: rec.CWD, Status: rec.Status, Since: session.StatusSince, Source: ReviveRegistry, Liveness: liveness,
			Resume: session.Resume, OpenJobs: ledger.OpenJobs(),
		})
	}

	ids, err := listLedgers(dir)
	if err != nil {
		diags = append(diags, warning(DiagLedgerUnreadable, err.Error()))
	}
	unproven := 0
	for _, id := range ids {
		if registered[id] {
			continue
		}
		ledger, err := loadLedger(dir, id)
		if err != nil {
			diags = append(diags, warning(DiagLedgerUnreadable, err.Error()))
			continue
		}
		if ledger.open() == 0 {
			continue
		}
		if ledger.Owner == nil {
			unproven++
			continue
		}
		liveness := view.classify(ledger.Owner.PID, ledger.Owner.ProcStart, ledger.Owner.PIDDomain, env.goos(), env.Now.Location())
		if !liveness.Dead() {
			if liveness == LivenessUnknown {
				unproven++
			}
			continue
		}
		project, root := roots.project(ledger.CWD)
		out.Sessions = append(out.Sessions, ReviveSession{
			SessionID: id, Project: project, Root: root, Worktree: worktreeLabel(ledger.CWD, root), CWD: ledger.CWD,
			Since: ledger.UpdatedAt, Source: ReviveLedger, Liveness: liveness,
			Resume: ResumeLine(ledger.CWD, id), OpenJobs: ledger.OpenJobs(),
		})
	}
	if unproven > 0 {
		diags = append(diags, runx.Diagnostic{Code: DiagLedgerUnproven, Severity: "info",
			Detail: fmt.Sprintf("%d ledger(s) with open jobs belong to sessions not proven dead; not offered", unproven)})
	}
	sort.SliceStable(out.Sessions, func(i, j int) bool { return out.Sessions[i].Since.After(out.Sessions[j].Since) })
	return out, diags, nil
}

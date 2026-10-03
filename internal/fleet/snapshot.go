package fleet

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/henderson-tech/vybava/internal/cmux"
	"github.com/henderson-tech/vybava/internal/runx"
)

// PublishedVersion is snapshot.json's contract version; bump it on a
// breaking change. Fleet.app refuses a snapshot of another version.
const PublishedVersion = 1

// Published is the full view `watch serve` writes to snapshot.json for
// Fleet.app: only sessions hosted in a cmux surface (D7), each with the
// actions the app may offer, waiting-first.
type Published struct {
	Version     int       `json:"version"`
	GeneratedAt time.Time `json:"generatedAt"`
	// CodexAt is when the Codex rows were read (they refresh slower).
	CodexAt *time.Time  `json:"codexAt,omitempty"`
	Cmux    cmux.Status `json:"cmux"`
	// Counts and Projects cover the listed Claude sessions.
	Counts   Counts        `json:"counts"`
	Projects []Project     `json:"projects"`
	Sessions []Hosted      `json:"sessions"`
	Codex    []HostedCodex `json:"codex"`
	// Hidden counts live sessions running outside cmux, which nothing can act on.
	Hidden      int               `json:"hidden"`
	Diagnostics []runx.Diagnostic `json:"diagnostics"`
}

// Surface is where a listed session lives in cmux — for display only;
// every action resolves it again by live pid.
type Surface struct {
	SurfaceID   string `json:"surfaceId"`
	WorkspaceID string `json:"workspaceId"`
}

// Hosted is a Claude session with its cmux surface and allowed actions.
type Hosted struct {
	Session
	Surface Surface  `json:"surface"`
	Actions []Action `json:"actions"`
}

// HostedCodex is a Codex process with its cmux surface — Focus only (D6).
type HostedCodex struct {
	CodexRow
	Surface Surface  `json:"surface"`
	Actions []Action `json:"actions"`
}

// PublishedPath is where watch serve publishes the view.
func PublishedPath(home string) string {
	return filepath.Join(home, ".local", "state", "vybava", "fleet", "snapshot.json")
}

// joinWorkers bounds concurrent resolve calls; cmux serves each on a pool slot.
const joinWorkers = 4

// Publisher builds and writes snapshot.json. Claude rows are read on every
// Publish; Codex rows (a ~10 s ps/lsof read) only on RefreshCodex.
type Publisher struct {
	// Env is the machine; its Now and Codex fields are ignored (Clock and
	// RefreshCodex own them).
	Env   Env
	Path  string
	Clock func() time.Time
	// ReadCodex reads live Codex rows; nil lists none.
	ReadCodex CodexReader

	mu        sync.Mutex
	codex     []CodexRow
	codexAt   time.Time
	codexDiag []runx.Diagnostic
	publish   sync.Mutex
}

// RefreshCodex re-reads the Codex rows the next Publish lists.
func (p *Publisher) RefreshCodex(ctx context.Context) {
	if p.ReadCodex == nil {
		return
	}
	now := p.Clock()
	rows, warnings, err := p.ReadCodex(ctx, now)
	var diags []runx.Diagnostic
	if err != nil {
		diags = append(diags, warning(DiagCodexUnavailable, err.Error()))
	}
	for _, w := range warnings {
		diags = append(diags, warning(DiagCodexPartial, w))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil {
		p.codex, p.codexAt = rows, now
	}
	p.codexDiag = diags
}

// Build reads the registry, joins every live session to its cmux surface
// and returns the view; it never writes.
func (p *Publisher) Build(ctx context.Context) (Published, error) {
	env := p.Env
	env.Now, env.Codex = p.Clock(), nil
	snap, diags, err := Read(ctx, env)
	if err != nil {
		return Published{}, err
	}
	p.mu.Lock()
	codex := append([]CodexRow(nil), p.codex...)
	codexAt := p.codexAt
	diags = append(diags, p.codexDiag...)
	p.mu.Unlock()
	for i := range codex {
		codex[i].Project, codex[i].Root = resolver{home: env.Home, cache: map[string][2]string{}}.project(codex[i].CWD)
	}

	out := Published{Version: PublishedVersion, GeneratedAt: env.Now, Projects: []Project{}, Sessions: []Hosted{}, Codex: []HostedCodex{}, Diagnostics: []runx.Diagnostic{}}
	if !codexAt.IsZero() {
		out.CodexAt = &codexAt
	}
	if env.Cmux == nil {
		out.Cmux = cmux.Status{State: cmux.StateUnreachable, Detail: "no cmux client configured"}
	} else {
		out.Cmux = env.Cmux.Check(ctx)
	}

	var live []Session
	for _, s := range snap.Sessions {
		if !s.Liveness.Dead() {
			live = append(live, s)
		}
	}
	if out.Cmux.State != cmux.StateOK && out.Cmux.State != cmux.StateRestricted {
		out.Hidden = len(live) + len(codex)
		out.Diagnostics = append(diags, warning(DiagCmuxUnavailable, out.Cmux.Detail))
		return out, nil
	}

	pids := make([]int, 0, len(live)+len(codex))
	for _, s := range live {
		pids = append(pids, s.PID)
	}
	for _, c := range codex {
		pids = append(pids, c.PID)
	}
	surfaces := p.join(ctx, env.Cmux, pids)

	projects := map[string]*Project{}
	for _, s := range live {
		surface, ok := surfaces[s.PID]
		if !ok {
			out.Hidden++
			continue
		}
		actions := []Action{ActionFocus, ActionScreen}
		if s.State == StateWaiting || s.State == StateIdle {
			actions = append(actions, ActionReply)
		}
		out.Sessions = append(out.Sessions, Hosted{Session: s, Surface: surface, Actions: actions})
		out.Counts.add(s.State)
		key := s.Root + "\x00" + s.Project
		if projects[key] == nil {
			projects[key] = &Project{Project: s.Project, Root: s.Root}
		}
		projects[key].Counts.add(s.State)
	}
	for _, c := range codex {
		surface, ok := surfaces[c.PID]
		if !ok {
			out.Hidden++
			continue
		}
		out.Codex = append(out.Codex, HostedCodex{CodexRow: c, Surface: surface, Actions: []Action{ActionFocus}})
	}
	for _, project := range projects {
		out.Projects = append(out.Projects, *project)
	}
	sort.Slice(out.Projects, func(i, j int) bool {
		a, b := out.Projects[i].Counts, out.Projects[j].Counts
		if a.Waiting != b.Waiting {
			return a.Waiting > b.Waiting
		}
		return out.Projects[i].Project < out.Projects[j].Project
	})
	out.Diagnostics = append(out.Diagnostics, diags...)
	return out, nil
}

// join resolves pids to surfaces, a few at a time. A pid no surface hosts
// is simply absent; a cmux error for one pid hides only that row.
func (p *Publisher) join(ctx context.Context, cx Cmux, pids []int) map[int]Surface {
	out := map[int]Surface{}
	var mu sync.Mutex
	work := make(chan int)
	var wg sync.WaitGroup
	for range min(joinWorkers, len(pids)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for pid := range work {
				target, err := cx.Resolve(ctx, pid)
				if err != nil {
					continue
				}
				mu.Lock()
				out[pid] = Surface{SurfaceID: target.SurfaceID, WorkspaceID: target.WorkspaceID}
				mu.Unlock()
			}
		}()
	}
	for _, pid := range pids {
		work <- pid
	}
	close(work)
	wg.Wait()
	return out
}

// Publish builds the view and writes it atomically; concurrent calls (a
// timer and a cmux event) are serialized.
func (p *Publisher) Publish(ctx context.Context) error {
	p.publish.Lock()
	defer p.publish.Unlock()
	view, err := p.Build(ctx)
	if err != nil {
		return err
	}
	return WritePublished(p.Path, view)
}

// Wakes reports whether a cmux event can change who waits: a turn starting
// or ending, a notification (input or permission needed), a session coming
// or going. Tool calls are left to the 15 s timer.
func Wakes(e cmux.Event) bool {
	switch e.Name {
	case "agent.hook.Notification", "agent.hook.Stop", "agent.hook.UserPromptSubmit",
		"agent.hook.SessionStart", "agent.hook.SessionEnd", "agent.hook.PermissionRequest":
		return true
	}
	return false
}

// Follow republishes within settle of every waking cmux event and after
// every gap, until ctx ends. follow is cmux.Client.Follow.
func (p *Publisher) Follow(ctx context.Context, follow func(context.Context, []string, func(cmux.Event), func(string)) error, settle time.Duration, onErr func(error)) error {
	wake := make(chan struct{}, 1)
	signal := func() {
		select {
		case wake <- struct{}{}:
		default:
		}
	}
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-wake:
			}
			// Coalesce a burst (Stop + Notification arrive together).
			select {
			case <-ctx.Done():
				return
			case <-time.After(settle):
			}
			if err := p.Publish(ctx); err != nil && onErr != nil {
				onErr(err)
			}
		}
	}()
	return follow(ctx, []string{"agent"}, func(e cmux.Event) {
		if Wakes(e) {
			signal()
		}
	}, func(string) { signal() })
}

// WritePublished writes the view so a reader never sees half a file.
func WritePublished(path string, view Published) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(view, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(body, '\n'))
}

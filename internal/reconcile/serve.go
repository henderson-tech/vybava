package reconcile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ListenAddr validates the address the read-only page binds to: it must be a
// concrete private (WireGuard-mesh) IP present on a local interface — never
// 0.0.0.0, never loopback, never a public address.
func ListenAddr(listen string) (string, error) {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "", fmt.Errorf("listen must be <wireguard-ip>:<port>: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "", fmt.Errorf("listen: %q is not an IP address (no hostnames — the bind must be explicit)", host)
	}
	if ip.IsUnspecified() || ip.IsLoopback() || !ip.IsPrivate() {
		return "", fmt.Errorf("listen: %s is not a private mesh address — refusing to bind", ip)
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(ip) {
			return net.JoinHostPort(host, port), nil
		}
	}
	return "", fmt.Errorf("listen: %s is not assigned to any local interface", ip)
}

// Server is the per-box read-only status page.
type Server struct {
	Engine *Engine
	// CacheTTL bounds how often a request triggers a status sweep.
	CacheTTL time.Duration

	mu     sync.Mutex
	cached *StatusReport
	at     time.Time
}

func (s *Server) report() (StatusReport, error) {
	ttl := s.CacheTTL
	if ttl == 0 {
		ttl = 10 * time.Second
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cached != nil && time.Since(s.at) < ttl {
		return *s.cached, nil
	}
	rep, err := s.Engine.StatusReport(50)
	if err != nil {
		return rep, err
	}
	s.cached, s.at = &rep, time.Now()
	return rep, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status.json", func(w http.ResponseWriter, r *http.Request) {
		rep, err := s.report()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rep)
	})
	mux.HandleFunc("GET /diff", func(w http.ResponseWriter, r *http.Request) {
		diff, err := s.Engine.Diff(r.URL.Query().Get("path"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, diff)
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		rep, err := s.report()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = boxPage.Execute(w, rep)
	})
	return readOnly(mux)
}

// readOnly refuses anything but GET/HEAD: the page never mutates a box.
func readOnly(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "read-only", http.StatusMethodNotAllowed)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Serve binds the handler to a validated mesh address until ctx is done.
func Serve(ctx context.Context, listen string, h http.Handler, log io.Writer) error {
	addr, err := ListenAddr(listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	fmt.Fprintf(log, "reconcile: serving read-only status on http://%s/\n", addr)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// ── hub ──────────────────────────────────────────────────────────────────────

// HubEntry is one polled box.
type HubEntry struct {
	Name    string        `json:"name"`
	URL     string        `json:"url"`
	Report  *StatusReport `json:"report,omitempty"`
	Error   string        `json:"error,omitempty"`
	Polled  time.Time     `json:"polled"`
	Latency string        `json:"latency,omitempty"`
}

// Hub polls each box's /status.json and renders one estate page.
type Hub struct {
	Hosts    []HubHost
	Interval time.Duration
	Client   *http.Client

	mu      sync.RWMutex
	entries []HubEntry
}

func (h *Hub) poll(ctx context.Context) {
	client := h.Client
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	entries := make([]HubEntry, len(h.Hosts))
	var wg sync.WaitGroup
	for i, host := range h.Hosts {
		wg.Add(1)
		go func(i int, host HubHost) {
			defer wg.Done()
			url := strings.TrimRight(host.URL, "/") + "/status.json"
			entry := HubEntry{Name: host.Name, URL: host.URL, Polled: time.Now()}
			start := time.Now()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				entry.Error = err.Error()
				entries[i] = entry
				return
			}
			resp, err := client.Do(req)
			if err != nil {
				entry.Error = err.Error()
				entries[i] = entry
				return
			}
			defer resp.Body.Close()
			entry.Latency = time.Since(start).Truncate(time.Millisecond).String()
			if resp.StatusCode != http.StatusOK {
				entry.Error = resp.Status
				entries[i] = entry
				return
			}
			var rep StatusReport
			if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rep); err != nil {
				entry.Error = "decode: " + err.Error()
			} else {
				entry.Report = &rep
			}
			entries[i] = entry
		}(i, host)
	}
	wg.Wait()
	h.mu.Lock()
	h.entries = entries
	h.mu.Unlock()
}

// Poll refreshes once, then every Interval until ctx is done.
func (h *Hub) Poll(ctx context.Context) {
	interval := h.Interval
	if interval == 0 {
		interval = 30 * time.Second
	}
	h.poll(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.poll(ctx)
		}
	}
}

func (h *Hub) Entries() []HubEntry {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return append([]HubEntry(nil), h.entries...)
}

func (h *Hub) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"generated_at": time.Now(), "hosts": h.Entries()})
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = hubPage.Execute(w, map[string]any{"Hosts": h.Entries(), "Now": time.Now()})
	})
	return readOnly(mux)
}

// ── templates ────────────────────────────────────────────────────────────────

var pageFuncs = template.FuncMap{
	"short": short,
	"since": func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		return time.Since(t).Truncate(time.Second).String() + " ago"
	},
}

const pageCSS = `body{font:14px/1.45 system-ui,sans-serif;margin:2rem;color:#1d1d1f;background:#fafafa}
h1{font-size:1.3rem}h2{font-size:1rem;margin-top:1.6rem}table{border-collapse:collapse;width:100%}
td,th{text-align:left;padding:.35rem .6rem;border-bottom:1px solid #e5e5e5;vertical-align:top}
code{font:12px ui-monospace,monospace;background:#f0f0f0;padding:.1rem .3rem;border-radius:3px}
.pill{display:inline-block;padding:.1rem .5rem;border-radius:999px;font-size:12px;font-weight:600}
.in-sync{background:#dcfce7;color:#166534}.pending{background:#fef9c3;color:#854d0e}
.held{background:#ffedd5;color:#9a3412}.errors{background:#fee2e2;color:#991b1b}.unknown{background:#e5e7eb;color:#374151}
.muted{color:#6b7280}pre{background:#f0f0f0;padding:.6rem;overflow:auto}`

var boxPage = template.Must(template.New("box").Funcs(pageFuncs).Parse(`<!doctype html><meta charset="utf-8">
<title>infra-reconcile · {{.Repo}}</title><style>` + pageCSS + `</style>
<h1>{{.Repo}} <span class="muted">({{.HostLabel}})</span> <span class="pill {{.Sync}}">{{.Sync}}</span></h1>
<p>mode <code>{{.Mode}}</code> · HEAD <code>{{short .Commit}}</code> {{.CommitSubject}}
{{if .Pin}}· <strong>pinned</strong> to <code>{{short .Pin}}</code> by rollback{{end}}
{{if .VersionMismatch}}· <span class="pill errors">{{.VersionMismatch}}</span>{{end}}</p>
<p>last good <code>{{if .LastGood}}{{short .LastGood}}{{else}}—{{end}}</code> {{.LastGoodSubject}}
· last tick {{if .LastTick}}{{since .LastTick.Time}} ({{.LastTick.Action}}, {{if .LastTick.OK}}ok{{else}}<strong>failed</strong>{{end}}){{else}}never{{end}}
· clone <code>{{.Clone}}</code> · generated {{.GeneratedAt.Format "2006-01-02 15:04:05"}}
· <a href="/status.json">status.json</a></p>
{{if .Errors}}<h2>Errors ({{len .Errors}})</h2><table>{{range .Errors}}<tr><td><span class="pill errors">{{.Kind}}</span></td><td>{{.Message}}</td></tr>{{end}}</table>{{end}}
{{if .Held}}<h2>HELD — hand-edited live, never overwritten ({{len .Held}})</h2><table>{{range .Held}}<tr><td><code>{{.}}</code></td><td><a href="/diff?path={{.}}">diff</a></td></tr>{{end}}</table>{{end}}
{{if .Pending}}<h2>Pending ({{len .Pending}})</h2><table>{{range .Pending}}<tr><td><code>{{.}}</code></td></tr>{{end}}</table>{{end}}
{{if .SkippedApps}}<h2>Repo apps with no live dir</h2><p class="muted">{{range .SkippedApps}}<code>{{.}}</code> {{end}}</p>{{end}}
<h2>History</h2>
<table><tr><th>when</th><th>action</th><th>commit</th><th>mode</th><th>result</th><th>detail</th></tr>
{{range .History}}<tr><td>{{.Time.Format "01-02 15:04:05"}}</td><td>{{.Action}}{{if .Path}} <code>{{.Path}}</code>{{end}}</td><td><code>{{short .Commit}}</code></td><td>{{.Mode}}</td>
<td>{{if .OK}}<span class="pill in-sync">ok</span>{{else}}<span class="pill errors">failed</span>{{end}}</td>
<td class="muted">{{if .Applied}}applied {{len .Applied}} {{end}}{{if .Pending}}pending {{len .Pending}} {{end}}{{if .Held}}held {{len .Held}} {{end}}{{if .Errors}}errors {{len .Errors}} {{end}}{{if .RollNotes}}roll: {{range .RollNotes}}{{.}} {{end}}{{end}}{{range .RollSteps}}<br><strong>{{.}}</strong>{{end}}</td></tr>{{end}}
</table>`))

var hubPage = template.Must(template.New("hub").Funcs(pageFuncs).Parse(`<!doctype html><meta charset="utf-8">
<title>infra-reconcile · estate</title><style>` + pageCSS + `</style>
<h1>infra-reconcile estate <span class="muted">{{.Now.Format "2006-01-02 15:04:05"}}</span> · <a href="/status.json">status.json</a></h1>
<table><tr><th>box</th><th>state</th><th>mode</th><th>HEAD</th><th>last good</th><th>last tick</th><th>pending</th><th>held</th><th>errors</th><th>page</th></tr>
{{range .Hosts}}<tr><td><strong>{{.Name}}</strong>{{if .Report}} <span class="muted">{{.Report.Repo}}</span>{{end}}</td>
{{if .Report}}<td><span class="pill {{.Report.Sync}}">{{.Report.Sync}}</span>{{if .Report.Pin}} <span class="pill held">pinned</span>{{end}}{{if .Report.VersionMismatch}} <span class="pill errors">version</span>{{end}}</td>
<td>{{.Report.Mode}}</td><td><code>{{short .Report.Commit}}</code></td><td><code>{{if .Report.LastGood}}{{short .Report.LastGood}}{{else}}—{{end}}</code></td>
<td>{{if .Report.LastTick}}{{since .Report.LastTick.Time}}{{if not .Report.LastTick.OK}} <span class="pill errors">failed</span>{{end}}{{else}}never{{end}}</td>
<td>{{len .Report.Pending}}</td><td>{{len .Report.Held}}</td><td>{{len .Report.Errors}}</td>
{{else}}<td><span class="pill unknown">unreachable</span></td><td colspan="7" class="muted">{{.Error}}</td>{{end}}
<td><a href="{{.URL}}">open</a> <span class="muted">{{.Latency}}</span></td></tr>{{end}}
</table>`))

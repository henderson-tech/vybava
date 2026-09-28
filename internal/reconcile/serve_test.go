package reconcile

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestListenAddrRefusesNonMesh(t *testing.T) {
	for _, bad := range []string{"0.0.0.0:9470", "127.0.0.1:9470", ":9470", "8.8.8.8:9470", "localhost:9470", "10.255.255.254:9470", "10.8.0.10"} {
		if _, err := ListenAddr(bad); err == nil {
			t.Errorf("ListenAddr(%q) accepted", bad)
		}
	}
}

func TestBoxPageAndStatusJSONAreReadOnly(t *testing.T) {
	b := newBox(t, map[string]string{"nginx/site.conf": "listen 80\n"})
	e := b.engine()
	_, _ = e.Run()
	mustT(t, os.WriteFile(b.root+"/opt/conf.d/site.conf", []byte("hand\n"), 0o644))
	srv := httptest.NewServer((&Server{Engine: e}).Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/status.json")
	mustT(t, err)
	var rep StatusReport
	mustT(t, json.NewDecoder(resp.Body).Decode(&rep))
	resp.Body.Close()
	if rep.Sync != "held" || rep.Repo != "fixture" || len(rep.History) != 1 {
		t.Fatalf("status.json = %+v", rep)
	}

	resp, err = http.Get(srv.URL + "/")
	mustT(t, err)
	page := readAll(t, resp)
	if !strings.Contains(page, "HELD") || !strings.Contains(page, "nginx/site.conf") || !strings.Contains(page, `class="pill held"`) {
		t.Fatalf("page:\n%s", page)
	}

	resp, err = http.Get(srv.URL + "/diff?path=nginx/site.conf")
	mustT(t, err)
	if d := readAll(t, resp); !strings.Contains(d, "-hand") {
		t.Fatalf("diff = %q", d)
	}
	resp, _ = http.Get(srv.URL + "/diff?path=../x")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("traversal diff status = %d", resp.StatusCode)
	}
	resp.Body.Close()

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req, _ := http.NewRequest(method, srv.URL+"/status.json", nil)
		resp, err := http.DefaultClient.Do(req)
		mustT(t, err)
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Fatalf("%s allowed (%d)", method, resp.StatusCode)
		}
	}
	// the page never wrote state or moved the checkout
	if (State{Dir: b.m.StateDir}).AlertMarker("telegram") != "" {
		t.Fatal("serve wrote alert state")
	}
	if got, _ := e.git().revParse("HEAD"); got != rep.Commit {
		t.Fatal("serve moved the checkout")
	}
}

func TestHubAggregatesBoxes(t *testing.T) {
	b := newBox(t, map[string]string{"scripts/a.sh": "a\n"})
	e := b.engine()
	_, _ = e.Run()
	boxSrv := httptest.NewServer((&Server{Engine: e}).Handler())
	defer boxSrv.Close()
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer dead.Close()

	h := &Hub{Hosts: []HubHost{{Name: "alpha", URL: boxSrv.URL}, {Name: "beta", URL: dead.URL}, {Name: "gone", URL: "http://127.0.0.1:1"}}, Interval: time.Hour}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.poll(ctx)
	entries := h.Entries()
	if len(entries) != 3 || entries[0].Report == nil || entries[0].Report.Sync != "in-sync" {
		t.Fatalf("entries = %+v", entries)
	}
	if entries[1].Error == "" || entries[2].Error == "" || entries[1].Report != nil {
		t.Fatalf("unreachable boxes not flagged: %+v", entries[1:])
	}
	hubSrv := httptest.NewServer(h.Handler())
	defer hubSrv.Close()
	resp, err := http.Get(hubSrv.URL + "/")
	mustT(t, err)
	page := readAll(t, resp)
	for _, want := range []string{"alpha", "beta", "gone", "unreachable", "in-sync"} {
		if !strings.Contains(page, want) {
			t.Fatalf("hub page lacks %q:\n%s", want, page)
		}
	}
	resp, err = http.Get(hubSrv.URL + "/status.json")
	mustT(t, err)
	var agg struct {
		Hosts []HubEntry `json:"hosts"`
	}
	mustT(t, json.NewDecoder(resp.Body).Decode(&agg))
	resp.Body.Close()
	if len(agg.Hosts) != 3 {
		t.Fatalf("hub status.json hosts = %d", len(agg.Hosts))
	}
}

func TestMetricsShape(t *testing.T) {
	out := Metrics(Result{Mode: "converge", Pending: []string{"a"}, Held: []string{"b", "c"}, CertHeld: []string{"d"}, Errors: []Issue{{}}, FailedHooks: []string{"nginx"}, LastGood: "abc"}, 42)
	for _, want := range []string{
		"infra_reconcile_last_tick_timestamp 42\n",
		"infra_reconcile_pending 1\n",
		"infra_reconcile_held 2\n",
		"infra_reconcile_cert_held 1\n",
		"infra_reconcile_errors 2\n",
		"infra_reconcile_mode_info{mode=\"converge\"} 1\n",
		"infra_reconcile_last_good_commit_info{sha=\"abc\"} 1\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("metrics lack %q:\n%s", want, out)
		}
	}
}

func readAll(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var b strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return b.String()
}

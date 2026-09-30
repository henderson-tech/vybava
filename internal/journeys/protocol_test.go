package journeys

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The subprocess exercises the real registry/stdin/receipt/checkpoint boundary.
// Its only side effects are markers inside a test-owned temporary directory.
func TestStartupAdapterProcess(t *testing.T) {
	if os.Getenv("JOURNEYS_TEST_ADAPTER") != "1" {
		return
	}
	var req Request
	if err := json.NewDecoder(os.Stdin).Decode(&req); err != nil {
		os.Exit(4)
	}
	log, err := os.OpenFile("calls", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		os.Exit(4)
	}
	_, err = log.WriteString(req.Operation + "\n")
	if err != nil || log.Close() != nil {
		os.Exit(4)
	}
	s := req.Plan.Snapshot
	_, running := os.Stat("running")
	s.Capabilities = map[string]bool{"api": running == nil}
	if running == nil && os.Getenv("JOURNEYS_TEST_DRIFT") == "1" {
		s.Origins = map[string]string{"api": "http://wrong-target"}
	}
	r := Receipt{Version: 1, OK: true, Operation: req.Operation, Snapshot: &s, Diagnostics: []Diagnostic{}, Next: []string{}}
	if req.Operation == "verify" {
		evidence := json.RawMessage(`{"observed":"fixture"}`)
		r.Snapshot = nil
		r.Verification = &Verification{PlanHash: req.Plan.Hash, RequestHash: Digest(req.Verification), Check: "fixture", Result: "pass", ObservedAt: time.Now().UTC(), Source: "fixture", Evidence: evidence, EvidenceHash: Digest(evidence), Scope: []string{"fixture only"}}
		switch os.Getenv("JOURNEYS_TEST_VERIFY") {
		case "fail":
			r.OK, r.Verification.Result = false, "fail"
			r.Diagnostics = append(r.Diagnostics, Diagnostic{Code: "OUTCOME_MISMATCH", Message: "fixture mismatch"})
		case "forged":
			r.Verification.RequestHash = "wrong"
		case "evidence":
			r.Verification.Evidence = json.RawMessage(`{"observed":"changed"}`)
		case "missing":
			r.Verification = nil
		}
	}
	if req.Operation == "runtime" {
		if err := os.WriteFile("running", nil, 0600); err != nil {
			os.Exit(4)
		}
	}
	if req.Operation == "readiness" {
		_, blocked := os.Stat("block-readiness")
		if blocked == nil {
			r.OK = false
			r.Diagnostics = append(r.Diagnostics, Diagnostic{Code: "CAPABILITY_MISSING", Message: "live readiness failed"})
		}
	}
	if err := json.NewEncoder(os.Stdout).Encode(r); err != nil {
		os.Exit(4)
	}
	if !r.OK {
		os.Exit(3)
	}
	os.Exit(0)
}

func startupFixture(t *testing.T) (*Library, Plan, Store) {
	t.Helper()
	l := fixture(t)
	p := planFor(t, l)
	p.Adapter, p.Operations = "test", []string{"runtime", "readiness"}
	p.Snapshot = Snapshot{Revision: "rev", SourceHash: "source", Namespace: "owned", TargetFingerprint: "target", Origins: map[string]string{"api": "http://owned-target"}, Capabilities: map[string]bool{"api": false}, Bindings: []Binding{
		{Actor: "customer", Device: "iphone", Platform: "ios", DriverPort: 8103, AppID: "app", NativeHash: "build", UserRef: "customer"},
		{Actor: "worker", Device: "android", Platform: "android", DriverPort: 8203, AppID: "app", NativeHash: "build", UserRef: "worker"},
	}}
	if err := p.Seal(l); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(Registry{Version: 1, Adapters: map[string]Adapter{"test": {Command: []string{exe, "-test.run=^TestStartupAdapterProcess$"}, Env: []string{"JOURNEYS_TEST_ADAPTER", "JOURNEYS_TEST_DRIFT", "JOURNEYS_TEST_VERIFY"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFile(filepath.Join(l.Root, ".journeys", "adapters.json"), b, true); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JOURNEYS_TEST_ADAPTER", "1")
	return l, p, Store{Root: t.TempDir()}
}

func TestValidateBindingsAllowsSeparateAppiumServers(t *testing.T) {
	s := Snapshot{Revision: "rev", SourceHash: "source", Namespace: "owned", TargetFingerprint: "target", Bindings: []Binding{
		{Actor: "customer", Device: "iphone", Platform: "ios", DriverPort: 8103, AppiumPort: 14003, AppID: "app", NativeHash: "build", UserRef: "customer"},
		{Actor: "worker", Device: "android", Platform: "android", DriverPort: 8203, AppiumPort: 14004, AppID: "app", NativeHash: "build", UserRef: "worker"},
	}}
	if err := ValidateBindings(s); err != nil {
		t.Fatal(err)
	}
	s.Bindings[1].AppiumPort = 80
	if err := ValidateBindings(s); err == nil {
		t.Fatal("unsafe Appium server port accepted")
	}
}

func TestStartupAllowsHealthChangesButRechecksReadinessOnResume(t *testing.T) {
	l, p, s := startupFixture(t)
	cps, err := s.Start(l.Root, l, p, true)
	if err != nil || len(cps) != 2 || cps[1].State != "done" {
		t.Fatalf("health change blocked startup: %+v %v", cps, err)
	}
	current := *cps[1].Receipt.Snapshot
	if sameSnapshot(p.Snapshot, current) {
		t.Fatal("coverage comparison ignored changed capabilities")
	}
	if err := os.WriteFile(filepath.Join(l.Root, "block-readiness"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	cps, err = s.Start(l.Root, l, p, true)
	if err == nil || len(cps) != 2 || cps[1].State != "pending" {
		t.Fatalf("resumed startup reused stale readiness: %+v %v", cps, err)
	}
	var saved Checkpoint
	if err := ReadJSON(filepath.Join(s.Root, "plan-"+p.ID, "readiness.json"), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.State != "pending" || len(saved.Receipt.Diagnostics) != 1 || saved.Receipt.Diagnostics[0].Code != "CAPABILITY_MISSING" {
		t.Fatalf("failed readiness receipt was lost on disk: %+v", saved)
	}
	calls, err := os.ReadFile(filepath.Join(l.Root, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(calls), "runtime\n") != 1 || strings.Count(string(calls), "readiness\n") != 2 {
		t.Fatalf("wrong replay behavior: %s", calls)
	}
}

func TestStartupRejectsOriginDriftBeforeNextOperation(t *testing.T) {
	l, p, s := startupFixture(t)
	t.Setenv("JOURNEYS_TEST_DRIFT", "1")
	cps, err := s.Start(l.Root, l, p, true)
	if err == nil || !strings.Contains(err.Error(), "PLAN_STALE") || len(cps) != 1 {
		t.Fatalf("origin drift accepted: %+v %v", cps, err)
	}
	calls, err := os.ReadFile(filepath.Join(l.Root, "calls"))
	if err != nil || strings.Contains(string(calls), "readiness\n") {
		t.Fatalf("next operation ran on changed target: %s %v", calls, err)
	}
}

func TestStartupPinsKeepEveryIdentityField(t *testing.T) {
	_, p, _ := startupFixture(t)
	for name, change := range map[string]func(*Snapshot){
		"revision":     func(s *Snapshot) { s.Revision = "changed" },
		"source":       func(s *Snapshot) { s.SourceHash = "changed" },
		"namespace":    func(s *Snapshot) { s.Namespace = "changed" },
		"target":       func(s *Snapshot) { s.TargetFingerprint = "changed" },
		"origin":       func(s *Snapshot) { s.Origins["api"] = "http://changed" },
		"device":       func(s *Snapshot) { s.Bindings[0].Device = "changed" },
		"actor":        func(s *Snapshot) { s.Bindings[0].Actor = "changed" },
		"user":         func(s *Snapshot) { s.Bindings[0].UserRef = "changed" },
		"company":      func(s *Snapshot) { s.Bindings[0].CompanyRef = "changed" },
		"employee":     func(s *Snapshot) { s.Bindings[0].EmployeeRef = "changed" },
		"native build": func(s *Snapshot) { s.Bindings[0].NativeHash = "changed" },
		"app":          func(s *Snapshot) { s.Bindings[0].AppID = "changed" },
		"session":      func(s *Snapshot) { s.Bindings[0].SessionID = "changed" },
		"port":         func(s *Snapshot) { s.Bindings[0].DriverPort++ },
		"platform":     func(s *Snapshot) { s.Bindings[0].Platform = "android" },
	} {
		t.Run(name, func(t *testing.T) {
			b, err := json.Marshal(p.Snapshot)
			if err != nil {
				t.Fatal(err)
			}
			var current Snapshot
			if err := json.Unmarshal(b, &current); err != nil {
				t.Fatal(err)
			}
			change(&current)
			if sameStartupPins(p.Snapshot, current) {
				t.Fatal("changed startup pin accepted")
			}
		})
	}
}

package journeys

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Registry lives outside Markdown. The executable is trusted repository code;
// documents can only select its registered id and closed operation vocabulary.
type Registry struct {
	Version  int                `json:"v"`
	Adapters map[string]Adapter `json:"adapters"`
}
type Adapter struct {
	Command []string `json:"command"`
	Env     []string `json:"env"`
}
type Request struct {
	Version          int             `json:"v"`
	Operation        string          `json:"operation"`
	Plan             *Plan           `json:"plan,omitempty"`
	Key              string          `json:"idempotencyKey,omitempty"`
	Publication      *Publication    `json:"publication,omitempty"`
	Verification     json.RawMessage `json:"verification,omitempty"`
	CaptureApprovals []string        `json:"captureApprovals,omitempty"`
}
type Receipt struct {
	Version      int           `json:"v"`
	OK           bool          `json:"ok"`
	Operation    string        `json:"operation"`
	Snapshot     *Snapshot     `json:"snapshot,omitempty"`
	Diagnostics  []Diagnostic  `json:"diagnostics"`
	Next         []string      `json:"next"`
	ReceiptRef   string        `json:"receiptRef,omitempty"`
	Publication  *Published    `json:"publication,omitempty"`
	Verification *Verification `json:"verification,omitempty"`
}
type cappedBuffer struct{ bytes.Buffer }

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 1024*1024 {
		return 0, fmt.Errorf("adapter output exceeds 1 MiB")
	}
	return b.Buffer.Write(p)
}
func Invoke(root, name string, request Request) (Receipt, error) {
	var registry Registry
	path, err := SafePath(root, ".journeys/adapters.json")
	if err != nil {
		return Receipt{}, problem("CAPABILITY_MISSING", "missing .journeys/adapters.json registry")
	}
	if err = ReadJSON(path, &registry); err != nil {
		return Receipt{}, err
	}
	a, ok := registry.Adapters[name]
	if registry.Version != 1 || !ok || len(a.Command) == 0 {
		return Receipt{}, problem("DOCUMENT_INVALID", "unregistered adapter")
	}
	if !slices.Contains([]string{"doctor", "devices", "snapshot", "runtime", "seed", "attach", "readiness", "publish", "verify"}, request.Operation) {
		return Receipt{}, problem("DOCUMENT_INVALID", "unsupported adapter operation")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, a.Command[0], a.Command[1:]...)
	cmd.Dir = root
	// Explicit allowlist, never inherit DATABASE_URL or profile-changing globals.
	for _, key := range append([]string{"PATH", "HOME", "TMPDIR"}, a.Env...) {
		if key == "" || strings.ContainsAny(key, "=\x00") {
			return Receipt{}, problem("DOCUMENT_INVALID", "invalid adapter environment key")
		}
		if value, exists := os.LookupEnv(key); exists {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	b, err := json.Marshal(request)
	if err != nil {
		return Receipt{}, err
	}
	cmd.Stdin = bytes.NewReader(b)
	var out, stderr cappedBuffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	runErr := cmd.Run()
	var receipt Receipt
	if err = Decode(out.Bytes(), &receipt); err != nil {
		return Receipt{}, problem("ADAPTER_PROTOCOL", "adapter did not return a valid receipt; raw output withheld")
	}
	if receipt.Version != 1 || receipt.Operation != request.Operation {
		return Receipt{}, problem("ADAPTER_PROTOCOL", "adapter receipt version/operation mismatch")
	}
	if runErr != nil || !receipt.OK {
		return receipt, problem("CAPABILITY_MISSING", "adapter operation blocked; inspect diagnostics")
	}
	return receipt, nil
}
func sameSnapshot(a, b Snapshot) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

// Startup is allowed to change observed health (runtime is what establishes
// it), but never source, target, origins or actor/device/build/session pins.
// Coverage deliberately keeps sameSnapshot's stricter freshness comparison.
func sameStartupPins(a, b Snapshot) bool {
	a.Capabilities, b.Capabilities = nil, nil
	return sameSnapshot(a, b)
}
func ValidateBindings(s Snapshot) error {
	if s.Revision == "" || s.SourceHash == "" || s.Namespace == "" || s.TargetFingerprint == "" {
		return problem("TARGET_UNSAFE", "snapshot must identify source and owned target")
	}
	if len(s.Bindings) < 2 {
		return problem("DEVICE_AMBIGUOUS", "bind at least two explicit devices")
	}
	devices, actors, ports := map[string]bool{}, map[string]bool{}, map[int]bool{}
	for _, b := range s.Bindings {
		if b.Device == "" || devices[b.Device] || actors[b.Actor] || b.DriverPort < 1024 || b.DriverPort > 65535 || ports[b.DriverPort] || (b.AppiumPort != 0 && (b.AppiumPort < 1024 || b.AppiumPort > 65535)) {
			return problem("DEVICE_AMBIGUOUS", "unique actors, devices and driver ports are required")
		}
		if b.NativeHash == "" || b.AppID == "" || b.UserRef == "" || !slices.Contains([]string{"ios", "android"}, b.Platform) {
			return problem("NATIVE_BUILD_STALE", "device build and actor identity must be pinned")
		}
		devices[b.Device] = true
		actors[b.Actor] = true
		ports[b.DriverPort] = true
	}
	if !actors["customer"] || !actors["worker"] {
		return problem("DEVICE_AMBIGUOUS", "customer and worker bindings are required")
	}
	return nil
}

type Checkpoint struct {
	PlanHash  string  `json:"planHash"`
	Operation string  `json:"operation"`
	State     string  `json:"state"`
	Receipt   Receipt `json:"receipt"`
}

// Start revalidates the snapshot before each write. An uncertain mutation is
// retried with the same key; adapters MUST reconcile that key before applying.
func (s Store) Start(root string, l *Library, p Plan, apply bool) ([]Checkpoint, error) {
	if !apply {
		return nil, problem("TARGET_UNSAFE", "start requires --apply for this saved plan")
	}
	if err := p.Validate(l); err != nil {
		return nil, err
	}
	if p.Hash != p.computedHash() {
		return nil, problem("PLAN_STALE", "plan was edited")
	}
	if err := ValidateBindings(p.Snapshot); err != nil {
		return nil, err
	}
	if len(p.Operations) == 0 || p.Operations[len(p.Operations)-1] != "readiness" {
		return nil, problem("DOCUMENT_INVALID", "startup must end with an explicit readiness check")
	}
	seenOperations := map[string]bool{}
	for _, op := range p.Operations {
		if !slices.Contains([]string{"runtime", "seed", "attach", "readiness"}, op) || seenOperations[op] {
			return nil, problem("DOCUMENT_INVALID", "invalid planned operation")
		}
		seenOperations[op] = true
	}
	initial, err := Invoke(root, p.Adapter, Request{Version: 1, Operation: "snapshot", Plan: &p})
	if err != nil {
		return nil, err
	}
	if initial.Snapshot == nil || !sameStartupPins(p.Snapshot, *initial.Snapshot) {
		return nil, problem("PLAN_STALE", "source, devices or target changed; create a new plan")
	}
	dir, err := s.dir("plan-" + p.ID)
	if err != nil {
		return nil, err
	}
	unlock, err := lock(dir)
	if err != nil {
		return nil, err
	}
	defer unlock()
	checkpoints := []Checkpoint{}
	for _, op := range p.Operations {
		path := filepath.Join(dir, op+".json")
		var cp Checkpoint
		err := ReadJSON(path, &cp)
		if err == nil && cp.PlanHash != p.Hash {
			return checkpoints, problem("PLAN_STALE", "checkpoint belongs to another plan")
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return checkpoints, err
		}
		// Readiness is a live read, not a mutation to replay. A resumed start
		// must not reuse yesterday's successful health receipt.
		if cp.State == "done" && op != "readiness" {
			checkpoints = append(checkpoints, cp)
			continue
		}
		current, err := Invoke(root, p.Adapter, Request{Version: 1, Operation: "snapshot", Plan: &p})
		if err != nil {
			return checkpoints, err
		}
		if current.Snapshot == nil || !sameStartupPins(p.Snapshot, *current.Snapshot) {
			return checkpoints, problem("PLAN_STALE", "source, devices or target changed; create a new plan")
		}
		cp = Checkpoint{PlanHash: p.Hash, Operation: op, State: "pending"}
		if err = privateJSON(path, cp, false); err != nil {
			return checkpoints, err
		}
		receipt, invokeErr := Invoke(root, p.Adapter, Request{Version: 1, Operation: op, Plan: &p, Key: p.Hash + ":" + op})
		cp.Receipt = receipt
		if invokeErr == nil {
			cp.State = "done"
		}
		if err = privateJSON(path, cp, false); err != nil {
			return checkpoints, err
		}
		checkpoints = append(checkpoints, cp)
		if invokeErr != nil {
			return checkpoints, invokeErr
		}
	}
	return checkpoints, nil
}

type Coverage struct {
	Authored       int            `json:"authoredScenarios"`
	Candidates     int            `json:"candidateModeCells"`
	Selected       int            `json:"originalDenominator"`
	Applicable     int            `json:"applicableDenominator"`
	Attempts       int            `json:"attempts"`
	Attempted      int            `json:"attemptedCells"`
	Observed       int            `json:"observedCells"`
	Counts         map[string]int `json:"verdicts"`
	Stale          int            `json:"staleAttempts"`
	Preflight      int            `json:"preflightAttempts"`
	FailedAttempts int            `json:"failedAttempts"`
	Superseded     int            `json:"supersededAttempts"`
}

func (s Store) Coverage(l *Library, p *Plan) (Coverage, error) {
	return s.CoverageAt(l, p, nil)
}

// A missing current snapshot is unknown impact, never evidence of freshness.
func (s Store) CoverageAt(l *Library, p *Plan, current *Snapshot) (Coverage, error) {
	r := Coverage{Authored: len(l.Scenarios), Candidates: l.CandidateCount(), Counts: map[string]int{}}
	for _, v := range verdicts {
		r.Counts[v] = 0
	}
	if p == nil {
		return r, nil
	}
	if err := p.Validate(l); err != nil {
		return r, err
	}
	r.Selected = len(p.Cells)
	r.Applicable = r.Selected
	fresh := current != nil && sameSnapshot(p.Snapshot, *current)
	r.Counts["stale"] = 0
	entries, err := os.ReadDir(s.Root)
	if errors.Is(err, os.ErrNotExist) {
		r.Counts["not-run"] = r.Selected
		return r, nil
	}
	if err != nil {
		return r, err
	}
	type latest struct {
		at     time.Time
		result CellResult
	}
	latestCells := map[string]latest{}
	attempted := map[string]bool{}
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), "plan-") {
			continue
		}
		a, _, _, err := s.Read(entry.Name(), false)
		if err != nil {
			return r, err
		}
		if a.Phase == "preflight" {
			r.Preflight++
			continue
		}
		if a.Plan.Hash != p.Hash {
			if a.Plan.LibraryHash != l.Hash {
				r.Stale++
			}
			continue
		}
		r.Attempts++
		if !fresh {
			r.Stale++
		}
		// Starting a bounded attempt does not undo a result for an untouched
		// cell. Events below invalidate a prior result for the cell they touch.
		if _, events, _, err := s.Read(entry.Name(), false); err != nil {
			return r, err
		} else {
			for _, e := range events {
				if e.Kind == "verdict" && e.Verdict == "not-run" {
					continue
				}
				attempted[e.Cell] = true
				old, ok := latestCells[e.Cell]
				if !ok || !a.At.Before(old.at) {
					latestCells[e.Cell] = latest{a.At, CellResult{ID: e.Cell, Verdict: "not-run"}}
				}
			}
		}
		var summary Summary
		if err := ReadJSON(filepath.Join(s.Root, entry.Name(), "summary.json"), &summary); errors.Is(err, os.ErrNotExist) {
			continue
		} else if err != nil {
			return r, err
		}
		verified, err := s.Finish(entry.Name())
		if err != nil {
			return r, err
		}
		summary = verified
		failed := false
		for _, c := range summary.Cells {
			failed = failed || c.Verdict == "fail"
			if c.Verdict == "not-run" {
				continue
			}
			attempted[c.ID] = true
			old, ok := latestCells[c.ID]
			if !ok || !summary.Started.Before(old.at) {
				latestCells[c.ID] = latest{summary.Started, c}
			}
		}
		if failed {
			r.FailedAttempts++
		}
		if a.RetestOf != "" {
			r.Superseded++
		}
	}
	r.Attempted = len(attempted)
	for _, cell := range p.Cells {
		latest, ok := latestCells[cell.ID]
		if !ok {
			r.Counts["not-run"]++
			continue
		}
		result := latest.result
		if result.Verdict == "pass" && !fresh {
			r.Counts["stale"]++
			continue
		}
		if result.Verdict == "pass" && (cell.Lane != "live-sandbox" || cell.Topology == "api-assisted") {
			r.Counts["blocked"]++
			continue
		}
		r.Counts[result.Verdict]++
		if result.Observed {
			r.Observed++
		}
		if result.Verdict == "not-applicable" {
			r.Applicable--
		}
	}
	return r, nil
}

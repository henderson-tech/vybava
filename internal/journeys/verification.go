package journeys

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/henderson-tech/vybava/internal/secretscan"
)

// The consumer adapter owns the closed request vocabulary and scoped evidence.
// A probe never marks a journey passed or silently appends a journal verdict.
type Verification struct {
	PlanHash     string          `json:"planHash"`
	RequestHash  string          `json:"requestHash"`
	Check        string          `json:"check"`
	Result       string          `json:"result"`
	ObservedAt   time.Time       `json:"observedAt"`
	Source       string          `json:"source"`
	Evidence     json.RawMessage `json:"evidence"`
	EvidenceHash string          `json:"evidenceHash"`
	Scope        []string        `json:"scope"`
}
type ProbeRecord struct {
	Version     int             `json:"v"`
	State       string          `json:"state"`
	OK          bool            `json:"ok"`
	PlanHash    string          `json:"planHash"`
	RequestHash string          `json:"requestHash"`
	Request     json.RawMessage `json:"request"`
	Receipt     Receipt         `json:"receipt"`
	Diagnostics []Diagnostic    `json:"diagnostics"`
}

func compactJSON(raw json.RawMessage) ([]byte, error) {
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		return nil, problem("DOCUMENT_INVALID", "invalid verification JSON")
	}
	return b.Bytes(), nil
}

// Probe saves an exclusive private receipt even when the live check blocks or
// fails. Retrying requires a new output, preserving the previous observation.
func Probe(root string, l *Library, p Plan, input json.RawMessage, output string) (ProbeRecord, error) {
	record := ProbeRecord{Version: 1, State: "pending", PlanHash: p.Hash, Diagnostics: []Diagnostic{}}
	if err := p.CheckSeal(); err != nil {
		return record, err
	}
	if err := p.Validate(l); err != nil {
		return record, err
	}
	if err := ValidateBindings(p.Snapshot); err != nil {
		return record, err
	}
	if len(input) == 0 || len(input) > 16384 {
		return record, problem("DOCUMENT_INVALID", "verification request must contain at most 16 KiB")
	}
	compact, err := compactJSON(input)
	if err != nil {
		return record, err
	}
	if len(secretscan.Find(string(compact), secretscan.All, nil)) > 0 {
		return record, problem("REDACTION_FAILED", "verification input contains credentials; use adapter environment references")
	}
	record.Request, record.RequestHash = compact, Digest(compact)
	path, err := PrivatePath(root, output)
	if err != nil {
		return record, err
	}
	// Reserve before invoking an adapter, so a reused path cannot consume a new
	// observation and then lose it to an existing immutable receipt.
	if err := privateJSON(path, record, true); err != nil {
		return record, err
	}
	r, probeErr := Invoke(root, p.Adapter, Request{Version: 1, Operation: "snapshot", Plan: &p})
	if probeErr == nil && (r.Snapshot == nil || !sameStartupPins(p.Snapshot, *r.Snapshot)) {
		probeErr = problem("PLAN_STALE", "verification source/target/device pins changed")
	}
	if probeErr == nil {
		r, probeErr = Invoke(root, p.Adapter, Request{Version: 1, Operation: "verify", Plan: &p, Verification: compact})
		if err := validateVerification(r, p.Hash, record.RequestHash); err != nil {
			probeErr = err
		} else if r.Verification != nil && r.Verification.Result == "fail" {
			probeErr = problem("OUTCOME_MISMATCH", "independent outcome did not match the expectation; inspect the private receipt")
		} else if r.Verification != nil && r.Verification.Result != "pass" {
			probeErr = problem("OUTCOME_UNSTABLE", "independent outcome is pending or unknown; inspect the private receipt")
		}
	}
	record.Receipt = r
	record.State, record.OK = "done", probeErr == nil
	if probeErr != nil {
		var p *Problem
		if errors.As(probeErr, &p) {
			record.Diagnostics = append(record.Diagnostics, p.Diagnostic)
		} else {
			record.Diagnostics = append(record.Diagnostics, Diagnostic{Code: "ADAPTER_PROTOCOL", Message: "verification invocation failed"})
		}
	}
	if err := privateJSON(path, record, false); err != nil {
		return record, err
	}
	return record, probeErr
}

func validateVerification(r Receipt, planHash, requestHash string) error {
	v := r.Verification
	if v == nil && !r.OK {
		return nil // A blocked prerequisite produces no business verdict.
	}
	if v == nil || v.PlanHash != planHash || v.RequestHash != requestHash || v.Check == "" || v.Source == "" || v.ObservedAt.IsZero() || len(v.Scope) == 0 || !slices.Contains([]string{"pass", "fail", "pending", "unknown"}, v.Result) || r.OK != (v.Result == "pass") {
		return problem("ADAPTER_PROTOCOL", "verification receipt is absent, unbound or inconsistent")
	}
	evidence, err := compactJSON(v.Evidence)
	if err != nil || len(evidence) == 0 || bytes.Equal(evidence, []byte("null")) || Digest(evidence) != v.EvidenceHash {
		return problem("ADAPTER_PROTOCOL", "verification evidence hash mismatch")
	}
	return nil
}

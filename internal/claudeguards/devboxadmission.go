package claudeguards

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Devbox admission for machine:devbox-workspace. In the three days to
// 2026-09-27 the rule fired 102 times and CLAUDE_GUARDS_ALLOW_LOCAL_STACK=1
// was typed 186 times: the box was saturated ("each run queues 10–30
// minutes"), so every refusal became the prefix, and then the prefix came
// pre-emptively. A rule whose escape rate is ~100% protects nothing. The rule
// now asks the devbox CLI whether the box can admit a run: it can → refuse as
// before; it cannot → the command runs here with a one-line note saying why.
// Unknown (no devbox on PATH, a slow or failing call, an unreadable answer)
// keeps refusing — routing stays the default whenever the box cannot be asked.
//
// The answer is `devbox status --json`, cached for admissionTTL in the temp
// dir: the overview's capacity.cpu.saturated IS admission's own CPU rule, a
// queue entry still `queued` means runs are waiting, memory below its floor
// refuses runs, and the CLI's own WS_CPU_SATURATED / WS_MAKE_ROOM / WS_HOT_FULL
// diagnostics name the refusal it would give.
// ---------------------------------------------------------------------------

const (
	admissionTTL     = 60 * time.Second
	admissionTimeout = 8 * time.Second
)

// devboxAdmission is the box's answer: Saturated when it cannot take a run,
// Reason naming why in the box's own terms. Unknown answers are !Saturated
// with an empty Reason.
type devboxAdmission struct {
	Saturated bool
	Reason    string
}

// admissionCachePath is where the last `devbox status --json` answer lives.
func admissionCachePath() string {
	return filepath.Join(os.TempDir(), "claude-guards-devbox-status.json")
}

// devboxStatusJSON returns the raw status document, from the cache when it is
// younger than admissionTTL, else from the CLI (and refreshes the cache). Any
// failure returns nil.
func devboxStatusJSON(now time.Time) []byte {
	cache := admissionCachePath()
	if st, err := os.Stat(cache); err == nil && now.Sub(st.ModTime()) < admissionTTL {
		if raw, err := os.ReadFile(cache); err == nil && len(raw) > 0 {
			return raw
		}
	}
	bin, err := exec.LookPath("devbox")
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), admissionTimeout)
	defer cancel()
	raw, err := exec.CommandContext(ctx, bin, "status", "--json").Output()
	if err != nil || len(raw) == 0 {
		return nil
	}
	_ = os.WriteFile(cache, raw, 0o600) // a failed cache write only costs the next call
	return raw
}

// judgeAdmission reads the box's capacity out of a status document.
func judgeAdmission(raw []byte) devboxAdmission {
	var doc struct {
		Data struct {
			Overview struct {
				Capacity struct {
					CPU struct {
						Saturated    bool    `json:"saturated"`
						PressureSome float64 `json:"pressureSome"`
						PSIMax       float64 `json:"psiMax"`
					} `json:"cpu"`
					Memory struct {
						HeadroomGB float64 `json:"headroomGB"`
						FloorGB    float64 `json:"floorGB"`
					} `json:"memory"`
				} `json:"capacity"`
				Queue []struct {
					State string `json:"state"`
				} `json:"queue"`
			} `json:"overview"`
		} `json:"data"`
		Diagnostics []struct {
			Code string `json:"code"`
		} `json:"diagnostics"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return devboxAdmission{}
	}
	var reasons []string
	cap := doc.Data.Overview.Capacity
	if cap.CPU.Saturated {
		reasons = append(reasons, fmt.Sprintf("CPU pressure %.0f over the %.0f admission ceiling", cap.CPU.PressureSome, cap.CPU.PSIMax))
	}
	queued := 0
	for _, q := range doc.Data.Overview.Queue {
		if q.State == "queued" {
			queued++
		}
	}
	if queued > 0 {
		reasons = append(reasons, fmt.Sprintf("%d run(s) already waiting in the queue", queued))
	}
	if cap.Memory.FloorGB > 0 && cap.Memory.HeadroomGB < cap.Memory.FloorGB {
		reasons = append(reasons, fmt.Sprintf("%.0f GB headroom under the %.0f GB floor", cap.Memory.HeadroomGB, cap.Memory.FloorGB))
	}
	for _, d := range doc.Diagnostics {
		switch d.Code {
		case "WS_CPU_SATURATED", "WS_MAKE_ROOM", "WS_HOT_FULL", "DEVBOX_NO_MAKE_ROOM", "WS_NETWORK_POOLS_EXHAUSTED":
			reasons = append(reasons, d.Code)
		}
	}
	if len(reasons) == 0 {
		return devboxAdmission{}
	}
	return devboxAdmission{Saturated: true, Reason: strings.Join(reasons, "; ")}
}

// currentAdmission is the seam the rule calls; tests replace it.
var currentAdmission = func() devboxAdmission {
	raw := devboxStatusJSON(time.Now())
	if raw == nil {
		return devboxAdmission{}
	}
	return judgeAdmission(raw)
}

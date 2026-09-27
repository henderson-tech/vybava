package claudeguards

import "testing"

// The box's own signals decide: admission's CPU rule, a waiting queue, memory
// under its floor or one of its refusal diagnostics; anything else, or an
// unreadable answer, is "unknown", which keeps the rule refusing.
func TestJudgeAdmission(t *testing.T) {
	for name, tc := range map[string]struct {
		raw       string
		saturated bool
		reason    string
	}{
		"idle box": {
			raw: `{"ok":true,"data":{"overview":{"capacity":{"cpu":{"saturated":false,"pressureSome":4.6,"psiMax":30},"memory":{"headroomGB":26,"floorGB":8}},"queue":[{"state":"running"}]}},"diagnostics":[]}`,
		},
		"cpu saturated": {
			raw:       `{"data":{"overview":{"capacity":{"cpu":{"saturated":true,"pressureSome":41.2,"psiMax":30},"memory":{"headroomGB":26,"floorGB":8}},"queue":[]}}}`,
			saturated: true, reason: "CPU pressure 41 over the 30 admission ceiling",
		},
		"runs waiting": {
			raw:       `{"data":{"overview":{"capacity":{"cpu":{},"memory":{"headroomGB":26,"floorGB":8}},"queue":[{"state":"running"},{"state":"queued"},{"state":"queued"}]}}}`,
			saturated: true, reason: "2 run(s) already waiting in the queue",
		},
		"memory under floor": {
			raw:       `{"data":{"overview":{"capacity":{"cpu":{},"memory":{"headroomGB":3,"floorGB":8}},"queue":[]}}}`,
			saturated: true, reason: "3 GB headroom under the 8 GB floor",
		},
		"refusal diagnostic": {
			raw:       `{"data":{"overview":{"capacity":{"cpu":{},"memory":{"headroomGB":26,"floorGB":8}}}},"diagnostics":[{"code":"WS_CONTEXT_MISMATCH"},{"code":"WS_MAKE_ROOM"}]}`,
			saturated: true, reason: "WS_MAKE_ROOM",
		},
		"unreadable": {raw: `not json`},
		"empty":      {raw: `{}`},
	} {
		got := judgeAdmission([]byte(tc.raw))
		if got.Saturated != tc.saturated || got.Reason != tc.reason {
			t.Errorf("%s: got %+v, want saturated=%v reason=%q", name, got, tc.saturated, tc.reason)
		}
	}
}

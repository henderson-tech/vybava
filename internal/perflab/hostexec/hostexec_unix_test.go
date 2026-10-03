//go:build !windows

package hostexec

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestFillPipe(t *testing.T) {
	cases := []struct {
		name     string
		capacity int // bytes the stub pipe accepts before EAGAIN
		want     int
	}{
		{"exhausted kernel pipe memory (the hang)", 512, 512},
		{"healthy fresh pipe", 65536, 65536},
		{"ample pipe stops at the cap", 4 << 20, PipeProbeCap},
		{"just under the floor", PipeCapacityLow - 1024, PipeCapacityLow - 1024},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			left := tc.capacity
			got, err := fillPipe(func(p []byte) (int, error) {
				if left == 0 {
					return 0, syscall.EAGAIN
				}
				n := min(len(p), left)
				left -= n
				return n, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("fillPipe = %d, want %d", got, tc.want)
			}
		})
	}
	if _, err := fillPipe(func([]byte) (int, error) { return 0, syscall.EBADF }); !errors.Is(err, syscall.EBADF) {
		t.Fatalf("a non-EAGAIN write error must surface, got %v", err)
	}
}

func TestPipeCapacityRealProbe(t *testing.T) {
	got, err := PipeCapacity()
	if err != nil {
		t.Fatal(err)
	}
	if got <= 0 || got > PipeProbeCap {
		t.Fatalf("PipeCapacity = %d, want 1..%d", got, PipeProbeCap)
	}
}

func TestOSRunStallStopsTheGroup(t *testing.T) {
	start := time.Now()
	res, err := OS{}.Run(context.Background(), Cmd{
		Argv:  []string{"/bin/sh", "-c", "echo started; sleep 30"},
		Stall: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Stalled || res.TimedOut {
		t.Fatalf("want stalled (not timed out), got %+v", res)
	}
	if time.Since(start) > 15*time.Second {
		t.Fatalf("the stall watchdog did not stop the child promptly (%s)", time.Since(start))
	}
	if !strings.Contains(string(res.Stdout), "started") {
		t.Fatalf("stdout lost: %q", res.Stdout)
	}
}

func TestOSRunTimeoutAndExit(t *testing.T) {
	res, err := OS{}.Run(context.Background(), Cmd{Argv: []string{"/bin/sh", "-c", "sleep 30"}, Timeout: 200 * time.Millisecond})
	if err != nil || !res.TimedOut {
		t.Fatalf("want timed out, got %+v %v", res, err)
	}
	res, err = OS{}.Run(context.Background(), Cmd{Argv: []string{"/bin/sh", "-c", "echo out; echo err >&2; exit 3"}})
	if err != nil || res.Exit != 3 || res.Tail() != "err" {
		t.Fatalf("want exit 3 tail err, got %+v %v", res, err)
	}
	if _, err := (OS{}).Run(context.Background(), Cmd{Argv: []string{"perflab-no-such-binary"}}); !NotFound(err) {
		t.Fatalf("want NotFound, got %v", err)
	}
}

func TestProgressLines(t *testing.T) {
	var buf bytes.Buffer
	now := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	p := NewProgress(&buf, "wda build", func() time.Time { return now })
	p.Phase("pipe-probe")
	now = now.Add(42 * time.Second)
	p.Phase("xcodebuild", "key=wda1-abc")
	want := "perflab[wda build] phase=pipe-probe +0s\nperflab[wda build] phase=xcodebuild key=wda1-abc +42s\n"
	if buf.String() != want {
		t.Fatalf("progress =\n%s\nwant\n%s", buf.String(), want)
	}
}

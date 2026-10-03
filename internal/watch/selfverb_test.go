package watch

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestMain lets this test binary stand in for the vybava binary selfVerb
// runs: WATCH_SELFVERB_HELPER picks what the child does.
func TestMain(m *testing.M) {
	switch os.Getenv("WATCH_SELFVERB_HELPER") {
	case "echo":
		fmt.Print(strings.Join(os.Args[1:], " "))
		os.Exit(0)
	case "block":
		// A descendant keeps stdout open after this child is killed.
		hold := exec.Command("sleep", "5")
		hold.Stdout = os.Stdout
		_ = hold.Start()
		time.Sleep(5 * time.Second)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestSelfVerbDispatchesTheGitkitVerb(t *testing.T) {
	t.Setenv("WATCH_SELFVERB_HELPER", "echo")
	var stdout, stderr bytes.Buffer
	code := selfVerb("merge-precheck")(context.Background(), []string{"155", "--repo=/w/vybava"}, &stdout, &stderr)
	if code != 0 || stdout.String() != "gitkit merge-precheck 155 --repo=/w/vybava" {
		t.Fatalf("child ran %q (exit %d, stderr %q)", stdout.String(), code, stderr.String())
	}
}

func TestSelfVerbReturnsAtItsDeadlineDespiteAHeldPipe(t *testing.T) {
	t.Setenv("WATCH_SELFVERB_HELPER", "block")
	defer func(d time.Duration) { selfVerbWaitDelay = d }(selfVerbWaitDelay)
	selfVerbWaitDelay = 200 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var stdout, stderr bytes.Buffer
	start := time.Now()
	code := selfVerb("merge-precheck")(ctx, []string{"155", "--repo=/w/vybava"}, &stdout, &stderr)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("a cancelled child held the probe for %s", elapsed)
	}
	if code == 0 {
		t.Fatalf("a killed child reported success (stderr %q)", stderr.String())
	}
}

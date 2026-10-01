//go:build !windows

package polishkit

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/henderson-tech/vybava/internal/runx"
)

// lockHelperEnv makes the test binary act as a lock holder: it takes the
// pass lock named by the variable, prints "locked" and sleeps until killed.
const lockHelperEnv = "POLISHKIT_LOCK_HELPER"

func TestHelperLockHolder(t *testing.T) {
	path := os.Getenv(lockHelperEnv)
	if path == "" {
		t.Skip("helper process only")
	}
	unlock, err := lockFile(path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	os.Stdout.WriteString("locked\n")
	time.Sleep(time.Hour)
}

// A bounded wait: a held lock makes Update answer ledger-locked naming the
// lock path, never hang. Then the lock held by a child process that is
// killed before it unlocks is acquired by the parent: the OS released it.
func TestLedgerLockIsBoundedAndSurvivesAKilledHolder(t *testing.T) {
	fx := &fakeExec{rules: gitRules("apps/client/app/home.tsx")}
	tool := newTool(t, testConfig(), fx)
	if _, err := tool.Init(context.Background(), InitOptions{Lanes: []string{"ios26"}, Screens: []string{"home"}}); err != nil {
		t.Fatal(err)
	}
	lock := filepath.Join(tool.PassDir(1), LockName)
	old := LockWait
	LockWait = 300 * time.Millisecond
	t.Cleanup(func() { LockWait = old })

	helper := exec.Command(os.Args[0], "-test.run=^TestHelperLockHolder$", "-test.v")
	helper.Env = append(os.Environ(), lockHelperEnv+"="+lock)
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	killed := false
	t.Cleanup(func() {
		if !killed {
			_ = helper.Process.Kill()
			_ = helper.Wait()
		}
	})
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		if scanner.Text() == "locked" {
			break
		}
	}
	// held by a live process: bounded wait, then the diagnostic
	start := time.Now()
	_, err = tool.Update(1, func(*RunFile) error { return nil })
	var de runx.DiagError
	if !errors.As(err, &de) || de.Diag.Code != DiagLedgerLocked || de.Diag.Fix != "wait for the other polish-kit command, or delete "+lock+" if no polish-kit process is running" {
		t.Fatalf("want ledger-locked: %v", err)
	}
	if waited := time.Since(start); waited < LockWait || waited > 5*time.Second {
		t.Fatalf("wait not bounded by LockWait: %s", waited)
	}
	// the holder dies without unlocking: the OS releases the lock
	if err := helper.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = helper.Wait()
	killed = true
	if _, err := tool.Update(1, func(run *RunFile) error { run.Cells[0].Note = "after"; return nil }); err != nil {
		t.Fatalf("lock of a killed holder must be free: %v", err)
	}
	run, _ := tool.LoadRun(1)
	if run.Cells[0].Note != "after" {
		t.Fatal("update after the kill not saved")
	}
}

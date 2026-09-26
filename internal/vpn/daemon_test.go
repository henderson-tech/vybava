package vpn

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestPlistStartsAtBootAndRestartsOnFailure(t *testing.T) {
	plist := Plist("lovinka-admin", "/opt/homebrew/bin")
	dec := xml.NewDecoder(strings.NewReader(plist))
	dec.Strict = true
	for {
		if _, err := dec.Token(); err == io.EOF {
			break
		} else if err != nil {
			t.Fatalf("plist is not well-formed XML: %v", err)
		}
	}
	compact := strings.Join(strings.Fields(plist), "")
	for _, want := range []string{
		"<key>Label</key><string>com.vybava.vpn.lovinka-admin</string>",
		"<string>/opt/homebrew/bin/bash</string><string>/usr/local/etc/vybava/wireguard/lovinka-admin.sh</string>",
		"<key>RunAtLoad</key><true/>",
		"<key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>",
		"<key>StandardOutPath</key><string>/var/log/vybava-vpn/lovinka-admin.log</string>",
	} {
		if !strings.Contains(compact, want) {
			t.Errorf("plist lacks %s", want)
		}
	}
	if strings.Contains(plist, "/tmp") {
		t.Error("the daemon must not depend on /tmp")
	}
}

func TestSupervisorIsValidBash(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	script := Supervisor("lovinka-admin", "/opt/homebrew/bin")
	cmd := exec.Command(bash, "-n")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bash -n: %v\n%s", err, out)
	}
	for _, want := range []string{
		"trap stop TERM INT", `wg-quick down "$conf"`, `wg-quick up "$conf"`, "conf='/usr/local/etc/vybava/wireguard/lovinka-admin.conf'",
		// a SIGKILLed wireguard-go leaves its .sock behind; the utun is the liveness truth
		`ifconfig "$iface" >/dev/null 2>&1; do`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("supervisor lacks %s", want)
		}
	}
}

// TestSupervisorLifecycle runs the rendered script against fake wg-quick,
// ifconfig and sleep: `bash -n` cannot see what it does at runtime.
func TestSupervisorLifecycle(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil || runtime.GOOS == "windows" {
		t.Skip("needs bash on a unix")
	}
	fakes := map[string]string{
		"wg-quick": `echo "$1" >> "$FAKE/calls"
case $1 in
up) [ ! -e "$FAKE/fail-up" ] || exit 1; (umask 377; echo utun99 > "$FAKE/run/lovinka-admin.name") ;;
down) /bin/rm -f "$FAKE/run/lovinka-admin.name" ;;
esac`,
		"ifconfig": `[ -e "$FAKE/alive" ]`,
		"sleep":    `exec /bin/sleep 0.05`,
	}
	cases := []struct {
		name                string
		failUp, sock, stale bool
		then                string // what happens once the tunnel is up: vanish | term
		exit                int
		calls, says         string
	}{
		{name: "a failed up exits 1 so launchd retries", failUp: true, exit: 1, calls: "up", says: "wg-quick up failed"},
		{name: "wireguard-go left no socket", exit: 1, calls: "up", says: "utun99 went away"},
		{name: "a killed wireguard-go: the utun vanishes, its socket stays", sock: true, then: "vanish", exit: 1, calls: "up", says: "utun99 went away"},
		{name: "a leftover run is cleared, and a stop runs wg-quick down", sock: true, stale: true, then: "term", exit: 0, calls: "down up down", says: "stopped"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fake, err := os.MkdirTemp("", "vpn") // short: a unix socket path is capped near 104 bytes
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(fake) })
			run, bin := filepath.Join(fake, "run"), filepath.Join(fake, "bin")
			if err := os.Mkdir(run, 0o755); err != nil {
				t.Fatal(err)
			}
			put := func(path, body string, mode os.FileMode) {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), mode); err != nil {
					t.Fatal(err)
				}
			}
			for tool, body := range fakes {
				put(filepath.Join(bin, tool), "#!/bin/sh\n"+body+"\n", 0o755)
			}
			put(filepath.Join(fake, "alive"), "", 0o644)
			if c.failUp {
				put(filepath.Join(fake, "fail-up"), "", 0o644)
			}
			if c.stale {
				put(filepath.Join(run, "lovinka-admin.name"), "utun99\n", 0o400)
			}
			if c.sock {
				l, err := net.Listen("unix", filepath.Join(run, "utun99.sock"))
				if err != nil {
					t.Fatal(err)
				}
				l.(*net.UnixListener).SetUnlinkOnClose(false)
				l.Close()
			}
			script, log := filepath.Join(fake, "supervisor.sh"), filepath.Join(fake, "log")
			put(script, supervisor("lovinka-admin", bin, run), 0o600)
			out, err := os.Create(log)
			if err != nil {
				t.Fatal(err)
			}
			defer out.Close()
			said := func() string { b, _ := os.ReadFile(log); return string(b) }
			cmd := exec.Command(bash, script)
			cmd.Env = append(os.Environ(), "FAKE="+fake)
			cmd.Stdout, cmd.Stderr = out, out
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			deadline := time.After(10 * time.Second)
			if c.then != "" {
				for !strings.Contains(said(), "up on utun99") {
					select {
					case <-deadline:
						cmd.Process.Kill()
						t.Fatalf("never came up:\n%s", said())
					case <-time.After(20 * time.Millisecond):
					}
				}
				if info, err := os.Stat(filepath.Join(run, "lovinka-admin.name")); err != nil || info.Mode().Perm() != 0o444 {
					t.Fatalf("status must be able to read the marker once up: %v %v", info, err)
				}
				if c.then == "vanish" {
					os.Remove(filepath.Join(fake, "alive"))
				} else {
					cmd.Process.Signal(syscall.SIGTERM)
				}
			}
			select {
			case err = <-done:
			case <-deadline:
				cmd.Process.Kill()
				t.Fatalf("the supervisor outlived its tunnel:\n%s", said())
			}
			exit := 0
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				exit = exitErr.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			calls, _ := os.ReadFile(filepath.Join(fake, "calls"))
			if got := strings.Join(strings.Fields(string(calls)), " "); exit != c.exit || got != c.calls || !strings.Contains(said(), c.says) {
				t.Fatalf("exit %d, wg-quick %q; want exit %d, wg-quick %q, saying %q:\n%s", exit, got, c.exit, c.calls, c.says, said())
			}
		})
	}
}

func TestInstallPlanKeepsTheKeyOutOfArgvAndReplacesARunningJob(t *testing.T) {
	config := "[Interface]\nPrivateKey = " + keyA + "\n"
	steps := InstallPlan("lovinka-admin", "/opt/homebrew/bin", config, true)
	rendered, err := json.Marshal(steps)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	fed := false
	for _, s := range steps {
		lines = append(lines, s.Command())
		fed = fed || (s.input == config && strings.Contains(s.Command(), "/usr/local/etc/vybava/wireguard/lovinka-admin.conf 0600"))
	}
	all := strings.Join(lines, "\n")
	if strings.Contains(all, keyA) || strings.Contains(string(rendered), keyA) {
		t.Fatal("private key rendered in the plan")
	}
	if !fed {
		t.Fatal("the profile must reach the 0600 config only through stdin")
	}
	bootout := strings.Index(all, "launchctl bootout system/com.vybava.vpn.lovinka-admin")
	bootstrap := strings.Index(all, "launchctl bootstrap system /Library/LaunchDaemons/com.vybava.vpn.lovinka-admin.plist")
	if bootout < 0 || bootstrap < bootout {
		t.Fatalf("a loaded job must be booted out before the daemon bootstraps:\n%s", all)
	}
	// Every file is in place before the running job stops, so a failed write
	// leaves the old tunnel — and the DNS it carries — up. The old supervisor
	// keeps its own script (writes rename), and darwin's `wg-quick down`
	// takes nothing from the new conf that Prepare admits.
	if lastWrite := strings.LastIndex(all, " vybava-vpn "); lastWrite < 0 || lastWrite > bootout {
		t.Fatalf("the files must be written before the running job is booted out:\n%s", all)
	}
	if !strings.Contains(all, "install -d -m 0700 -o root -g wheel /usr/local/etc/vybava/wireguard") {
		t.Fatalf("config dir is not root-only:\n%s", all)
	}
	if fresh := InstallPlan("lovinka-admin", "/opt/homebrew/bin", config, false); len(fresh) != len(steps)-1 {
		t.Fatal("nothing to boot out on a fresh install")
	}
}

func TestUninstallPlanIsIdempotent(t *testing.T) {
	steps := UninstallPlan("lovinka-admin", false)
	if len(steps) != 1 || steps[0].Command() != "sudo /bin/rm -f /Library/LaunchDaemons/com.vybava.vpn.lovinka-admin.plist /usr/local/etc/vybava/wireguard/lovinka-admin.sh /usr/local/etc/vybava/wireguard/lovinka-admin.conf" {
		t.Fatalf("unexpected plan: %#v", steps)
	}
	if loaded := UninstallPlan("lovinka-admin", true); loaded[0].Argv[1] != "bootout" {
		t.Fatal("a loaded daemon is stopped first")
	}
}

package vpn

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"os/exec"
	"strings"
	"testing"
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

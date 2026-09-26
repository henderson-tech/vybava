package vpn

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Every persistent tunnel is three root-owned files plus a log. The config
// dir is 0700 root:wheel: the private key rests only there, never in /tmp.
const (
	ConfigDir = "/usr/local/etc/vybava/wireguard"
	DaemonDir = "/Library/LaunchDaemons"
	LogDir    = "/var/log/vybava-vpn"
	// runDir is wg-quick's macOS state: <name>.name holds the utun, <utun>.sock
	// is wireguard-go's control socket. Both are root-only files in a
	// world-listable dir, so existence is observable without root.
	runDir = "/var/run/wireguard"
)

func Label(name string) string      { return "com.vybava.vpn." + name }
func ConfigPath(name string) string { return filepath.Join(ConfigDir, name+".conf") }
func ScriptPath(name string) string { return filepath.Join(ConfigDir, name+".sh") }
func PlistPath(name string) string  { return filepath.Join(DaemonDir, Label(name)+".plist") }
func LogPath(name string) string    { return filepath.Join(LogDir, name+".log") }

// HomebrewBin finds the Homebrew bin that holds everything the daemon runs.
func HomebrewBin() (string, error) {
	for _, dir := range []string{"/opt/homebrew/bin", "/usr/local/bin"} {
		missing := false
		for _, tool := range []string{"bash", "wg-quick", "wireguard-go"} {
			if _, err := os.Stat(filepath.Join(dir, tool)); err != nil {
				missing = true
			}
		}
		if !missing {
			return dir, nil
		}
	}
	return "", errors.New("no Homebrew bin holds bash, wg-quick and wireguard-go; run: brew install bash wireguard-tools wireguard-go")
}

func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func xmlText(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

// Plist is the tunnel's LaunchDaemon: started at boot, restarted 30 s after
// any unexpected exit (a failed `wg-quick up` before the network is up, a
// vanished interface), left stopped only after a clean stop.
func Plist(name, bin string) string {
	x := func(s string) string { return "<string>" + xmlText(s) + "</string>" }
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>` + x(Label(name)) + `
	<key>ProgramArguments</key>
	<array>
		` + x(filepath.Join(bin, "bash")) + `
		` + x(ScriptPath(name)) + `
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>` + x(bin+":/usr/bin:/bin:/usr/sbin:/sbin") + `
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ThrottleInterval</key>
	<integer>30</integer>
	<key>StandardOutPath</key>` + x(LogPath(name)) + `
	<key>StandardErrorPath</key>` + x(LogPath(name)) + `
</dict>
</plist>
`
}

// Supervisor is the daemon's program: it clears a leftover interface, brings
// the tunnel up with wg-quick and lives exactly as long as the interface, so
// launchd's state IS the tunnel's state. A stop (bootout) runs wg-quick down.
// Liveness is the utun itself (wg-quick's own monitor test), not only the
// socket file: a killed wireguard-go leaves its .sock behind.
func Supervisor(name, bin string) string {
	return `# vybava vpn supervisor for ` + name + ` — written by "vybava vpn install ` + name + `"; do not edit.
set -uo pipefail
export PATH=` + quote(bin+":/usr/bin:/bin:/usr/sbin:/sbin") + `
conf=` + quote(ConfigPath(name)) + `
marker=` + quote(filepath.Join(runDir, name+".name")) + `
iface=''
now() { date -u +%Y-%m-%dT%H:%M:%SZ; }
stop() {
	trap - TERM INT
	[[ -n $iface || ! -f $marker ]] || iface=$(< "$marker")
	if [[ -n $iface && -S /var/run/wireguard/$iface.sock ]]; then
		wg-quick down "$conf" || echo "$(now) wg-quick down failed; inspect routes" >&2
	fi
	echo "$(now) stopped"
	exit 0
}
trap stop TERM INT
if [[ -f $marker ]]; then
	stale=$(< "$marker")
	echo "$(now) clearing $stale left by an earlier run"
	if [[ -S /var/run/wireguard/$stale.sock ]]; then
		wg-quick down "$conf" || /bin/rm -f "/var/run/wireguard/$stale.sock"
	fi
	/bin/rm -f "$marker"
fi
if ! wg-quick up "$conf"; then
	echo "$(now) wg-quick up failed; launchd retries in 30 s" >&2
	exit 1
fi
iface=$(< "$marker")
echo "$(now) up on $iface"
while [[ -S /var/run/wireguard/$iface.sock ]] && ifconfig "$iface" >/dev/null 2>&1; do
	sleep 5 &
	wait $!
done
echo "$(now) $iface went away; launchd restarts the tunnel" >&2
exit 1
`
}

// Step is one privileged command, run as `sudo <argv>` by the human's own
// invocation. Stdin describes what is piped in; the bytes never render.
type Step struct {
	Why   string   `json:"why"`
	Argv  []string `json:"argv"`
	Stdin string   `json:"stdin,omitempty"`
	input string
}

// Command is the step as a copyable shell line.
func (s Step) Command() string {
	parts := []string{"sudo"}
	for _, a := range s.Argv {
		if strings.ContainsAny(a, " '\"$&|;<>*?()[]{}\\`") {
			a = quote(a)
		}
		parts = append(parts, a)
	}
	line := strings.Join(parts, " ")
	if s.Stdin != "" {
		line += "  < " + s.Stdin
	}
	return line
}

// writeFile atomically replaces path with the step's stdin at mode, root-owned.
func writeFile(path, mode, why, stdin, input string) Step {
	return Step{
		Why:   why,
		Argv:  []string{"/bin/sh", "-c", `umask 077 && /bin/cat > "$1.tmp" && /bin/chmod "$2" "$1.tmp" && /bin/mv -f "$1.tmp" "$1"`, "vybava-vpn", path, mode},
		Stdin: stdin, input: input,
	}
}

// InstallPlan is every privileged step that makes the tunnel persistent.
// loaded says a job already holds the label — the transient `launchctl
// submit` recovery or an earlier install — which is booted out first, so a
// re-install is a restart with the vault's current profile.
func InstallPlan(name, bin, config string, loaded bool) []Step {
	target := "system/" + Label(name)
	steps := []Step{
		{Why: "root-only home for the tunnel config", Argv: []string{"/usr/bin/install", "-d", "-m", "0700", "-o", "root", "-g", "wheel", ConfigDir}},
		{Why: "daemon log directory", Argv: []string{"/usr/bin/install", "-d", "-m", "0755", "-o", "root", "-g", "wheel", LogDir}},
		writeFile(ConfigPath(name), "0600", "the validated vault profile", "<profile from Onyx>", config),
		writeFile(ScriptPath(name), "0600", "the supervisor the daemon runs", "<supervisor script>", Supervisor(name, bin)),
		writeFile(PlistPath(name), "0644", "the LaunchDaemon", "<LaunchDaemon plist>", Plist(name, bin)),
	}
	if loaded {
		steps = append(steps, Step{Why: "stop the job holding the tunnel now", Argv: []string{"/bin/launchctl", "bootout", target}})
	}
	return append(steps,
		Step{Why: "clear a disabled override", Argv: []string{"/bin/launchctl", "enable", target}},
		Step{Why: "start the daemon (and at every boot)", Argv: []string{"/bin/launchctl", "bootstrap", "system", PlistPath(name)}},
	)
}

// UninstallPlan stops the tunnel (the supervisor runs wg-quick down) and
// removes its files; every step is a no-op when already gone.
func UninstallPlan(name string, loaded bool) []Step {
	var steps []Step
	if loaded {
		steps = append(steps, Step{Why: "stop the tunnel", Argv: []string{"/bin/launchctl", "bootout", "system/" + Label(name)}})
	}
	return append(steps, Step{Why: "remove the daemon, its supervisor and the private key", Argv: []string{"/bin/rm", "-f", PlistPath(name), ScriptPath(name), ConfigPath(name)}})
}

// Run executes the plan through sudo, which prompts on the terminal once.
func Run(ctx context.Context, steps []Step, stderr io.Writer) error {
	for _, s := range steps {
		cmd := exec.CommandContext(ctx, "sudo", s.Argv...)
		cmd.Stdin = strings.NewReader(s.input)
		cmd.Stdout, cmd.Stderr = stderr, stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s failed (%w): %s", s.Why, err, s.Command())
		}
	}
	return nil
}

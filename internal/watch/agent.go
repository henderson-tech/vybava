package watch

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// AgentLabel is the LaunchAgent that runs `vybava watch serve`.
const AgentLabel = "vybava.watchd"

// AgentPlistPath is where the LaunchAgent lives.
func AgentPlistPath(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", AgentLabel+".plist")
}

// Step is one install step, rendered by --dry-run and executed as the user
// (a LaunchAgent needs no sudo): either a command or an atomic file write.
type Step struct {
	Why     string   `json:"why"`
	Argv    []string `json:"argv,omitempty"`
	Write   string   `json:"write,omitempty"`
	Mode    string   `json:"mode,omitempty"`
	content string
}

// Command is the step as a copyable shell line.
func (s Step) Command() string {
	if s.Write != "" {
		return fmt.Sprintf("write %s (mode %s)", s.Write, s.Mode)
	}
	parts := make([]string, 0, len(s.Argv))
	for _, a := range s.Argv {
		if strings.ContainsAny(a, " '\"$&|;<>*?()[]{}\\`") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

// AgentPlist renders the LaunchAgent: `<bin> watch serve` at login, kept
// alive, logging to ~/Library/Logs/vybava/watchd.log. PATH is the
// installing shell's: launchd's own lacks gh, devbox, vitrinka and deployik.
func AgentPlist(bin string, paths Paths, pathEnv string) string {
	esc := func(s string) string {
		var b bytes.Buffer
		_ = xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	log := filepath.Join(paths.Logs, "watchd.log")
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- written by "vybava watch agent install"; do not edit -->
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + AgentLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + esc(bin) + `</string>
		<string>watch</string>
		<string>serve</string>
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>` + esc(pathEnv) + `</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>30</integer>
	<key>ProcessType</key>
	<string>Background</string>
	<key>StandardOutPath</key>
	<string>` + esc(log) + `</string>
	<key>StandardErrorPath</key>
	<string>` + esc(log) + `</string>
</dict>
</plist>
`
}

func agentTarget(uid int) string { return "gui/" + strconv.Itoa(uid) + "/" + AgentLabel }

// AgentInstallPlan writes the LaunchAgent and (re)starts it. loaded says a
// job already holds the label; it is booted out after the new plist is
// written, so a re-install is a restart onto the new binary.
func AgentInstallPlan(home string, uid int, bin, pathEnv string, loaded bool) []Step {
	paths := DefaultPaths(home)
	plist := AgentPlistPath(home)
	steps := []Step{
		{Why: "the daemon's state dir (socket and state, user-only)", Argv: []string{"/bin/mkdir", "-p", "-m", "0700", paths.Dir}},
		{Why: "the daemon's log dir", Argv: []string{"/bin/mkdir", "-p", paths.Logs}},
		{Why: "the LaunchAgent dir", Argv: []string{"/bin/mkdir", "-p", filepath.Dir(plist)}},
		{Why: "the LaunchAgent", Write: plist, Mode: "0644", content: AgentPlist(bin, paths, pathEnv)},
	}
	if loaded {
		steps = append(steps, Step{Why: "stop the running daemon", Argv: []string{"/bin/launchctl", "bootout", agentTarget(uid)}})
	}
	return append(steps,
		Step{Why: "clear a disabled override", Argv: []string{"/bin/launchctl", "enable", agentTarget(uid)}},
		Step{Why: "start the daemon (and at every login)", Argv: []string{"/bin/launchctl", "bootstrap", "gui/" + strconv.Itoa(uid), plist}},
	)
}

// AgentUninstallPlan stops the daemon and removes the LaunchAgent; the
// state (subscriptions, undelivered events) stays for a reinstall.
func AgentUninstallPlan(home string, uid int, loaded bool) []Step {
	var steps []Step
	if loaded {
		steps = append(steps, Step{Why: "stop the daemon", Argv: []string{"/bin/launchctl", "bootout", agentTarget(uid)}})
	}
	return append(steps, Step{Why: "remove the LaunchAgent", Argv: []string{"/bin/rm", "-f", AgentPlistPath(home)}})
}

// AgentLoaded asks launchd whether the label is loaded.
func AgentLoaded(ctx context.Context, uid int) bool {
	return exec.CommandContext(ctx, "/bin/launchctl", "print", agentTarget(uid)).Run() == nil
}

// RunPlan executes a plan as the user; a failing step stops it and names
// the step and its command.
func RunPlan(ctx context.Context, steps []Step, stderr io.Writer) error {
	for _, s := range steps {
		if s.Write != "" {
			if err := writeAtomic(s.Write, s.content, s.Mode); err != nil {
				return fmt.Errorf("%s failed: %w", s.Why, err)
			}
			continue
		}
		cmd := exec.CommandContext(ctx, s.Argv[0], s.Argv[1:]...)
		cmd.Stdout, cmd.Stderr = stderr, stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s failed (%w): %s", s.Why, err, s.Command())
		}
	}
	return nil
}

func writeAtomic(path, content, mode string) error {
	perm, err := strconv.ParseUint(mode, 8, 32)
	if err != nil {
		return fmt.Errorf("mode %q: %w", mode, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(os.FileMode(perm)); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

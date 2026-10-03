package doctor

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/henderson-tech/vybava/internal/catalog"
	"github.com/henderson-tech/vybava/internal/claudeguards"
	"github.com/henderson-tech/vybava/internal/state"
)

type Status string

const (
	StatusPass Status = "pass"
	StatusWarn Status = "warn"
	StatusFail Status = "fail"
)

type Check struct {
	ID      string `json:"id"`
	Status  Status `json:"status"`
	Message string `json:"message"`
	Remedy  string `json:"remedy,omitempty"`
}

type Report struct {
	Checks []Check `json:"checks"`
}

func (r Report) Healthy() bool {
	for _, check := range r.Checks {
		if check.Status == StatusFail {
			return false
		}
	}
	return true
}

func Run(c catalog.Catalog, store state.Store) Report {
	report := Report{Checks: []Check{{
		ID: "catalog", Status: StatusPass,
		Message: "embedded catalog schema and payload references are valid",
	}}}

	executable, err := os.Executable()
	if err != nil {
		report.Checks = append(report.Checks, Check{ID: "executable", Status: StatusFail, Message: err.Error()})
	} else {
		report.Checks = append(report.Checks, Check{ID: "executable", Status: StatusPass, Message: executable})
	}
	current, stateErr := store.Load()
	if stateErr != nil {
		report.Checks = append(report.Checks, Check{ID: "state", Status: StatusFail, Message: stateErr.Error(), Remedy: "repair or remove the invalid Výbava state file"})
	} else {
		report.Checks = append(report.Checks, Check{ID: "state", Status: StatusPass, Message: store.Path})
	}

	home, err := os.UserHomeDir()
	if err != nil {
		report.Checks = append(report.Checks, Check{ID: "home", Status: StatusFail, Message: err.Error()})
	} else {
		binDirs := make(map[string]struct{})
		for _, installed := range current.Installed {
			if installed.Kind == string(catalog.KindApplet) {
				binDirs[filepath.Dir(installed.Destination)] = struct{}{}
			}
		}
		if len(binDirs) == 0 {
			binDirs[filepath.Join(home, ".local", "bin")] = struct{}{}
		}
		for binDir := range binDirs {
			if pathContains(binDir) {
				report.Checks = append(report.Checks, Check{ID: "path:" + binDir, Status: StatusPass, Message: binDir + " is on PATH"})
			} else {
				report.Checks = append(report.Checks, Check{
					ID: "path:" + binDir, Status: StatusWarn, Message: binDir + " is not on PATH",
					Remedy: "add " + binDir + " to PATH so installed applets are directly callable",
				})
			}
		}
	}

	if _, err := exec.LookPath("git"); err != nil {
		report.Checks = append(report.Checks, Check{ID: "git", Status: StatusWarn, Message: "git is unavailable", Remedy: "install git before using git workflow skills"})
	} else {
		report.Checks = append(report.Checks, Check{ID: "git", Status: StatusPass, Message: "git is available"})
	}
	if _, err := exec.LookPath("gh"); err != nil {
		report.Checks = append(report.Checks, Check{ID: "gh", Status: StatusWarn, Message: "GitHub CLI is unavailable", Remedy: "install and authenticate gh before using PR skills"})
	} else {
		report.Checks = append(report.Checks, Check{ID: "gh", Status: StatusPass, Message: "GitHub CLI is available"})
	}

	if stateErr != nil {
		return report
	}
	known := make(map[string]struct{}, len(c.Items))
	for _, item := range c.Items {
		known[item.ID] = struct{}{}
	}
	for _, installed := range current.Installed {
		id := "installed:" + installed.ItemID
		if installed.Agent != "" {
			id += ":" + installed.Agent
		}
		if _, exists := known[installed.ItemID]; !exists {
			report.Checks = append(report.Checks, Check{ID: id, Status: StatusWarn, Message: "installed item is no longer in the catalog: " + installed.ItemID})
			continue
		}
		if _, err := os.Lstat(installed.Destination); errors.Is(err, os.ErrNotExist) {
			report.Checks = append(report.Checks, Check{ID: id, Status: StatusFail, Message: "installed item is missing at " + installed.Destination, Remedy: "run vybava update"})
		} else if err != nil {
			report.Checks = append(report.Checks, Check{ID: id, Status: StatusFail, Message: err.Error()})
		} else {
			report.Checks = append(report.Checks, Check{ID: id, Status: StatusPass, Message: installed.Destination})
		}
	}
	report.Checks = append(report.Checks, modChecks(current.Installed, claudeValidate)...)
	report.Checks = append(report.Checks, guardHooksCheck())
	return report
}

// errNoClaude stands for a machine without the claude CLI: a mod there is
// reported unvalidated, never failed.
var errNoClaude = errors.New("claude is not on PATH")

// claudeValidate runs `claude plugin validate` on an installed mod: the
// engine's own reading of the manifest and hooks module, the check that
// catches a mod an API change broke before a session refuses it.
func claudeValidate(dir string) ([]byte, error) {
	claude, err := exec.LookPath("claude")
	if err != nil {
		return nil, errNoClaude
	}
	return exec.Command(claude, "plugin", "validate", dir).CombinedOutput()
}

func modChecks(installed []state.Installed, validate func(dir string) ([]byte, error)) []Check {
	var checks []Check
	for _, item := range installed {
		if item.Kind != string(catalog.KindMod) {
			continue
		}
		if _, err := os.Stat(item.Destination); err != nil {
			continue // the installed:<id> check already reports it missing
		}
		id := "mod:" + item.ItemID
		output, err := validate(item.Destination)
		var exit *exec.ExitError
		switch {
		case err == nil:
			checks = append(checks, Check{ID: id, Status: StatusPass, Message: "claude plugin validate passed for " + item.Destination})
		case errors.As(err, &exit):
			checks = append(checks, Check{
				ID: id, Status: StatusFail, Message: "claude plugin validate failed: " + failureLine(output),
				Remedy: "claude plugin validate " + item.Destination + " — then vybava update " + item.ItemID,
			})
		default:
			checks = append(checks, Check{ID: id, Status: StatusWarn, Message: "mod not validated: " + err.Error(), Remedy: "install Claude Code, then vybava doctor"})
		}
	}
	return checks
}

// failureLine picks the reason out of validate's report, which opens with
// "Validating plugin manifest: <path>": the first ✘ line, else the last line.
func failureLine(output []byte) string {
	last := "no output"
	for _, line := range strings.Split(string(output), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "✘") {
			return line
		}
		if line != "" {
			last = line
		}
	}
	return last
}

// guardHooksCheck verifies ~/.claude/settings.json still wires every
// claude-guards hook — a harness rewrite dropped them once for 36 h.
func guardHooksCheck() Check {
	missing, err := claudeguards.CheckHooks("")
	switch {
	case errors.Is(err, claudeguards.ErrHooksMissing):
		var names []string
		for _, w := range missing {
			names = append(names, w.Event+" → "+w.Command)
		}
		return Check{ID: "claude-guards hooks", Status: StatusFail, Message: "settings.json is missing " + strings.Join(names, ", "), Remedy: "claude-guards doctor --fix"}
	case errors.Is(err, os.ErrNotExist):
		return Check{ID: "claude-guards hooks", Status: StatusWarn, Message: "no ~/.claude/settings.json", Remedy: "claude-guards doctor --fix"}
	case err != nil:
		return Check{ID: "claude-guards hooks", Status: StatusWarn, Message: err.Error()}
	}
	return Check{ID: "claude-guards hooks", Status: StatusPass, Message: "every claude-guards hook is wired in ~/.claude/settings.json"}
}

func pathContains(expected string) bool {
	for _, entry := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.Clean(entry) == filepath.Clean(expected) {
			return true
		}
	}
	return false
}

func FormatText(report Report) string {
	var output strings.Builder
	for _, check := range report.Checks {
		prefix := "PASS"
		switch check.Status {
		case StatusWarn:
			prefix = "WARN"
		case StatusFail:
			prefix = "FAIL"
		}
		output.WriteString(prefix + "  " + check.ID + " — " + check.Message + "\n")
		if check.Remedy != "" {
			output.WriteString("      remedy: " + check.Remedy + "\n")
		}
	}
	return output.String()
}

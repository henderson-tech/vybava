package catalog

import (
	"errors"
	"fmt"
	"strings"
)

// Tool is the recipe for an external app or CLI: how to see it is there
// (Probe), where it comes from (Install), and the guided step a human runs
// once it is (Setup). Every tool is installed by its product's own published
// channel — Výbava orchestrates, it never re-implements an installer.
type Tool struct {
	Probe   Probe   `yaml:"probe" json:"probe"`
	Install Install `yaml:"install" json:"install"`
	// Setup is the guided commands a fresh install owes, run in order.
	Setup [][]string `yaml:"setup,omitempty" json:"setup,omitempty"`
	Needs []string   `yaml:"needs,omitempty" json:"needs,omitempty"`
	// Optional tools start unchecked in `vybava setup team`.
	Optional bool `yaml:"optional,omitempty" json:"optional,omitempty"`
	// Interactive installs need a human at the terminal (a guided
	// onboarding, a browser approval); non-interactive runs report them as
	// next steps instead of running them.
	Interactive bool `yaml:"interactive,omitempty" json:"interactive,omitempty"`
}

// Probe names exactly one live check.
type Probe struct {
	// App is an .app bundle name looked up in /Applications and ~/Applications.
	App string `yaml:"app,omitempty" json:"app,omitempty"`
	// Command is an executable looked up on PATH and in ~/.local/bin.
	Command string `yaml:"command,omitempty" json:"command,omitempty"`
	// Path is a file whose existence proves the install; ~ expands to home.
	Path string `yaml:"path,omitempty" json:"path,omitempty"`
}

// Install names exactly one channel.
type Install struct {
	BrewCask string `yaml:"brew_cask,omitempty" json:"brew_cask,omitempty"`
	Brew     string `yaml:"brew,omitempty" json:"brew,omitempty"`
	// Pultik is an apps.fixit.app shelf app id; {arch} expands to arm64|amd64.
	Pultik string `yaml:"pultik,omitempty" json:"pultik,omitempty"`
	// Bun is a package installed globally with `bun add -g`.
	Bun string `yaml:"bun,omitempty" json:"bun,omitempty"`
	// Run is the product's own idempotent installer, re-run to update.
	Run []string `yaml:"run,omitempty" json:"run,omitempty"`
}

// Channel returns the one configured channel name and its target.
func (i Install) Channel() (string, string) {
	switch {
	case i.BrewCask != "":
		return "brew_cask", i.BrewCask
	case i.Brew != "":
		return "brew", i.Brew
	case i.Pultik != "":
		return "pultik", i.Pultik
	case i.Bun != "":
		return "bun", i.Bun
	case len(i.Run) > 0:
		return "run", strings.Join(i.Run, " ")
	}
	return "", ""
}

func (t *Tool) validate() error {
	if t == nil {
		return errors.New("kind tool needs a tool recipe")
	}
	if count(t.Probe.App != "", t.Probe.Command != "", t.Probe.Path != "") != 1 {
		return errors.New("probe must name exactly one of app, command, path")
	}
	i := t.Install
	if count(i.BrewCask != "", i.Brew != "", i.Pultik != "", i.Bun != "", len(i.Run) > 0) != 1 {
		return errors.New("install must name exactly one of brew_cask, brew, pultik, bun, run")
	}
	for i, command := range t.Setup {
		if len(command) == 0 || command[0] == "" {
			return fmt.Errorf("setup command %d is empty", i+1)
		}
	}
	if t.Probe.App != "" && !strings.HasSuffix(t.Probe.App, ".app") {
		return fmt.Errorf("probe app %q must end in .app", t.Probe.App)
	}
	return nil
}

func count(flags ...bool) int {
	n := 0
	for _, f := range flags {
		if f {
			n++
		}
	}
	return n
}

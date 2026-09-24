package installer

import (
	"path/filepath"
	"strings"
)

// CaskName is the Homebrew cask every workstation installs Výbava from.
const CaskName = "henderson-tech/tap/vybava"

// BrewCask reports whether a resolved executable path belongs to the
// Homebrew cask — the only install `vybava upgrade` can move forward.
// Source builds and CI installs (ci/install.sh) pin their own version.
func BrewCask(resolved string) bool {
	return strings.Contains(filepath.ToSlash(resolved), "/Caskroom/vybava/")
}

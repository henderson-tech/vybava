package installer

import "testing"

func TestBrewCaskOnlyMatchesTheCaskroom(t *testing.T) {
	if !BrewCask("/opt/homebrew/Caskroom/vybava/0.5.0/vybava") {
		t.Fatal("cask binary not recognised")
	}
	for _, path := range []string{"/Users/me/.local/bin/vybava", "/usr/local/bin/vybava", "/tmp/Caskroom/other/vybava"} {
		if BrewCask(path) {
			t.Fatalf("%s misread as the cask", path)
		}
	}
}

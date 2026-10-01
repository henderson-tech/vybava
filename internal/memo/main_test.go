package memo

import (
	"os"
	"testing"

	"github.com/henderson-tech/vybava/internal/devboxguest"
	"github.com/henderson-tech/vybava/internal/gittest"
)

// noGuestMarker is where tests point devboxguest.Marker: the suite runs on a
// Devbox guest too (`devbox run verify`), and every home is judged as on the
// Mac unless a test opts in.
const noGuestMarker = "/nonexistent/devbox-guest-marker"

// TestMain keeps every git these tests run, directly or through the code
// under test, from leaving auto maintenance writing into a t.TempDir
// repository while its cleanup deletes it (see gittest).
func TestMain(m *testing.M) {
	gittest.NoDaemons()
	devboxguest.Marker = noGuestMarker
	os.Exit(m.Run())
}

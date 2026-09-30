package gitkit

import (
	"os"
	"testing"

	"github.com/henderson-tech/vybava/internal/gittest"
)

// TestMain keeps every git these tests run, directly or through the code
// under test, from leaving auto maintenance writing into a t.TempDir
// repository while its cleanup deletes it (see gittest).
func TestMain(m *testing.M) {
	gittest.NoDaemons()
	os.Exit(m.Run())
}

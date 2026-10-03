package memorylint_test

import (
	"os"
	"testing"

	"github.com/henderson-tech/vybava/internal/gittest"
)

// TestMain keeps the git these tests commit with from leaving auto
// maintenance writing into a t.TempDir repository while its cleanup deletes
// it (see gittest).
func TestMain(m *testing.M) {
	gittest.NoDaemons()
	os.Exit(m.Run())
}

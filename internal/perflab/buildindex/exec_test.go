package buildindex

import (
	"context"
	"testing"

	"github.com/henderson-tech/vybava/internal/perflab/hostexec"
)

func TestRunMapsAMissingToolToItsInstallLine(t *testing.T) {
	_, err := run(context.Background(), hostexec.OS{}, Cmd{Argv: []string{"/nonexistent/zipalign", "-p"}})
	d := wantCode(t, err, DiagToolMissing)
	if d.Diag.Fix != toolInstall["zipalign"] {
		t.Fatalf("fix = %q", d.Diag.Fix)
	}
}

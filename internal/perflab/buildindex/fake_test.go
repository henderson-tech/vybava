package buildindex

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/henderson-tech/vybava/internal/runx"
)

// fakeRunner answers commands from scripted steps (first match on a
// substring of the joined argv; once-steps are consumed) and records every
// call. An unscripted command fails the test.
type fakeRunner struct {
	t     *testing.T
	mu    sync.Mutex
	steps []*fakeStep
	calls []string
}

type fakeStep struct {
	match string
	res   Result
	do    func(c Cmd) Result
	once  bool
	used  bool
}

func newFake(t *testing.T) *fakeRunner { return &fakeRunner{t: t} }

// on scripts a command matching substring; do, when set, computes the
// result (and performs the side effect a real tool would).
func (f *fakeRunner) on(match string, res Result) *fakeRunner {
	f.steps = append(f.steps, &fakeStep{match: match, res: res})
	return f
}

func (f *fakeRunner) onDo(match string, do func(c Cmd) Result) *fakeRunner {
	f.steps = append(f.steps, &fakeStep{match: match, do: do})
	return f
}

func (f *fakeRunner) once(match string, res Result) *fakeRunner {
	f.steps = append(f.steps, &fakeStep{match: match, res: res, once: true})
	return f
}

func (f *fakeRunner) Run(_ context.Context, c Cmd) (Result, error) {
	f.mu.Lock()
	line := strings.Join(c.Argv, " ")
	f.calls = append(f.calls, line)
	var hit *fakeStep
	for _, s := range f.steps {
		if (s.once && s.used) || !strings.Contains(line, s.match) {
			continue
		}
		hit = s
		break
	}
	if hit != nil && hit.once {
		hit.used = true
	}
	f.mu.Unlock()
	if hit == nil {
		f.t.Errorf("unscripted command: %s", line)
		return Result{Exit: 127}, nil
	}
	if hit.do != nil {
		return hit.do(c), nil
	}
	return hit.res, nil
}

func (f *fakeRunner) called(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

func ok(stdout string) Result { return Result{Stdout: []byte(stdout)} }

// wantCode asserts err carries the diagnostic code.
func wantCode(t *testing.T, err error, code string) runx.DiagError {
	t.Helper()
	var d runx.DiagError
	if !errors.As(err, &d) {
		t.Fatalf("want diagnostic %s, got %v", code, err)
	}
	if d.Diag.Code != code {
		t.Fatalf("want diagnostic %s, got %s: %s", code, d.Diag.Code, d.Diag.Detail)
	}
	return d
}

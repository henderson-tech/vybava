package polishkit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/henderson-tech/vybava/internal/runx"
)

// ShootOptions are the shoot verb's flags.
type ShootOptions struct {
	Lane    string
	Pass    int
	Screens []string
	Themes  []string
	Nav     []string
	Text    []string
}

// ShotRecord is one capture.
type ShotRecord struct {
	Cell string `json:"cell"`
	File string `json:"file"`
}

// ShootData is what shoot reports.
type ShootData struct {
	Lane  string       `json:"lane"`
	Pass  int          `json:"pass"`
	Shots []ShotRecord `json:"shots"`
	// Restored lists the reset commands run at the end.
	Restored []string `json:"restored"`
}

// Shoot captures every chrome cell of a lane: per device state (theme, nav,
// text size) set the device, then per screen open the deep link, wait its
// settle time and screenshot into <pass>/shots/<lane>/. Verdicts stay
// pending: the judgement is the agent's. Device state is restored at the end.
func (t *Tool) Shoot(ctx context.Context, opts ShootOptions) (Result, error) {
	l, ok := t.Config.Lane(opts.Lane)
	if !ok {
		return Result{}, diag(DiagUnknownLane, fmt.Sprintf("lane %q is not declared (lanes: %s)", opts.Lane, strings.Join(t.Config.LaneIDs(nil), ", ")), "polish-kit lanes --json")
	}
	run, err := t.LoadRun(opts.Pass)
	if err != nil {
		return Result{}, err
	}
	shotsDir := filepath.Join(run.PassDir, "shots", l.ID)
	switch l.Kind {
	case KindIOSDevice:
		return Result{}, diag(DiagLaneUnsupported,
			fmt.Sprintf("a phone is shot by hand (Volume Up + Side, AirDrop to the Mac) or by the repo's Appium recorder; drop the files as %s/<screen>--<theme>.png", shotsDir),
			fmt.Sprintf("polish-kit cell <lane>--<screen>--<theme> pass|fail --shot %s/<screen>--<theme>.png --pass %d --json", shotsDir, run.Pass))
	case KindBrowser:
		return Result{}, diag(DiagLaneUnsupported, "browser lanes are captured by ui-loop (Playwright); it owns web capture", "vybava ui-loop run --json")
	case KindServer:
		return Result{}, diag(DiagLaneUnsupported, "a server lane has nothing to shoot; its cells are matrix cells judged from responses and logs", fmt.Sprintf("polish-kit run add-cell --kind matrix --lane %s --flow \"<title>\" --tier \"<tier>\" --pass %d --json", l.ID, run.Pass))
	}
	cells, err := t.selectCells(run, l, opts)
	if err != nil {
		return Result{}, err
	}
	if len(cells) == 0 {
		return Result{}, diag(DiagUnknownScreen, fmt.Sprintf("pass %d has no chrome cell for lane %s matching the selection", run.Pass, l.ID), fmt.Sprintf("polish-kit run init --pass %d --lanes %s --force --json", run.Pass, l.ID))
	}
	st := t.Resolve(ctx, l)
	if st.problem != nil {
		return Result{}, runx.DiagError{Diag: *st.problem}
	}
	if !st.Booted {
		return Result{}, diag(DiagDeviceUnavailable, l.ID+" is not booted", st.Boot)
	}
	if err := os.MkdirAll(shotsDir, 0o755); err != nil {
		return Result{}, err
	}
	data := ShootData{Lane: l.ID, Pass: run.Pass, Shots: []ShotRecord{}, Restored: []string{}}
	var current *deviceState
	for _, idx := range cells {
		c := &run.Cells[idx]
		want := deviceState{Theme: c.Theme, Nav: c.Nav, Text: c.TextSize}
		if c.TextSize == "" {
			want.Text = defaultTextSize(l.Kind)
		}
		if current == nil || *current != want {
			if _, err := t.applyState(ctx, st, want); err != nil {
				return Result{Data: data}, err
			}
			current = &want
			t.Sleep(500 * time.Millisecond)
		}
		screen, _ := screenOf(run, c.Screen)
		file := filepath.Join(run.PassDir, filepath.FromSlash(c.ShotFile()))
		if err := t.capture(ctx, st, screen, file); err != nil {
			return Result{Data: data}, err
		}
		c.Shot = c.ShotFile()
		data.Shots = append(data.Shots, ShotRecord{Cell: c.ID, File: c.Shot})
		fmt.Fprintf(t.Log, "shot %s\n", c.Shot)
		if err := t.SaveRun(run); err != nil {
			return Result{Data: data}, err
		}
	}
	restored, err := t.applyState(ctx, st, deviceState{Reset: true})
	data.Restored = restored
	if err != nil {
		return Result{Data: data}, err
	}
	lines := []string{fmt.Sprintf("%d shots into %s", len(data.Shots), shotsDir)}
	next := []string{fmt.Sprintf("polish-kit sheet --pass %d --lanes %s --json", run.Pass, l.ID)}
	if pending := pendingCells(run); len(pending) > 0 {
		next = append(next, cellCommand(pending[0], run.Pass))
	}
	return Result{Data: data, Lines: lines, Next: next}, nil
}

func defaultTextSize(kind LaneKind) string {
	if kind == KindIOSSim {
		return "medium"
	}
	return "1.0"
}

func screenOf(run *RunFile, id string) (Screen, bool) {
	for _, s := range run.Screens {
		if s.ID == id {
			return s, true
		}
	}
	return Screen{}, false
}

// selectCells picks the lane's chrome cells (indices into run.Cells) that
// match the selection, in table order.
func (t *Tool) selectCells(run *RunFile, l Lane, opts ShootOptions) ([]int, error) {
	for _, id := range opts.Screens {
		if _, ok := screenOf(run, id); !ok {
			return nil, diag(DiagUnknownScreen, fmt.Sprintf("pass %d has no screen %q", run.Pass, id), fmt.Sprintf("polish-kit status --pass %d --json", run.Pass))
		}
	}
	for _, th := range opts.Themes {
		if !slices.Contains(Themes, th) {
			return nil, diag(DiagUsage, fmt.Sprintf("theme %q is not light or dark", th), "polish-kit shoot "+l.ID+" --themes light,dark --json")
		}
	}
	for _, n := range opts.Nav {
		if !slices.Contains(NavModes, n) {
			return nil, diag(DiagUsage, fmt.Sprintf("nav %q is not gesture or 3button", n), "polish-kit shoot "+l.ID+" --nav gesture --json")
		}
	}
	var out []int
	for i, c := range run.Cells {
		if c.Kind != CellChrome || c.Lane != l.ID {
			continue
		}
		if len(opts.Screens) > 0 && !slices.Contains(opts.Screens, c.Screen) {
			continue
		}
		if len(opts.Themes) > 0 && !slices.Contains(opts.Themes, c.Theme) {
			continue
		}
		if len(opts.Nav) > 0 && !slices.Contains(opts.Nav, c.Nav) {
			continue
		}
		if len(opts.Text) > 0 && !slices.Contains(opts.Text, orDefault(c.TextSize, "default")) {
			continue
		}
		out = append(out, i)
	}
	return out, nil
}

// capture opens the screen and screenshots it.
func (t *Tool) capture(ctx context.Context, st LaneState, screen Screen, file string) error {
	settle := time.Duration(screen.SettleMs) * time.Millisecond
	switch st.Kind {
	case KindIOSSim:
		if err := t.mustRun(ctx, "xcrun", "simctl", "openurl", st.UDID, screen.URL); err != nil {
			return err
		}
		t.Sleep(settle)
		return t.mustRun(ctx, "xcrun", "simctl", "io", st.UDID, "screenshot", "--type=png", file)
	case KindAndroidDevice, KindAndroidEmulator:
		// adb shell joins its arguments for the device's sh: quote the URL so
		// a query string's & never backgrounds am.
		if err := t.mustRun(ctx, "adb", "-s", st.Serial, "shell", "am", "start", "-a", "android.intent.action.VIEW", "-d", "'"+strings.ReplaceAll(screen.URL, "'", "")+"'"); err != nil {
			return err
		}
		t.Sleep(settle)
		out, err := t.run(ctx, 30*time.Second, "adb", "-s", st.Serial, "exec-out", "screencap", "-p")
		if err != nil {
			return err
		}
		if out.Code != 0 || len(out.Stdout) == 0 {
			return fmt.Errorf("adb screencap: %s", stderrTail(out))
		}
		return os.WriteFile(file, []byte(out.Stdout), 0o644)
	}
	return diag(DiagLaneUnsupported, string(st.Kind)+" lanes are not shot natively", "")
}

func (t *Tool) mustRun(ctx context.Context, args ...string) error {
	out, err := t.run(ctx, 30*time.Second, args...)
	if err != nil {
		return err
	}
	if out.Code != 0 {
		return fmt.Errorf("%s: %s", strings.Join(args, " "), stderrTail(out))
	}
	return nil
}

package xctrace

import (
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// FrameFamily is one row of the classification table: a sample belongs to
// the families its stack frames match, deepest first.
type FrameFamily struct {
	Name string
	Re   *regexp.Regexp
}

// FrameFamilies is the classification table ported from the calendar
// campaign's classify.ts, in match priority order (the first family a frame
// matches wins for that frame).
var FrameFamilies = []FrameFamily{
	{"a11y-automation", regexp.MustCompile(`UIViewAccessibility automationElements|_accessibilityUserTesting|AXRuntime|accessibilityElements`)},
	{"glass", regexp.MustCompile(`Glass|_UIVisualEffect|UIVisualEffect|Backdrop|_UIPortal`)},
	{"text-draw", regexp.MustCompile(`RCTTextLayoutManager drawAttributedString|NSTextStorage|NSLayoutManager|CTLine|CTFrame|TextKit|_NSCoreType|RCTParagraphComponentView drawRect`)},
	{"text-measure", regexp.MustCompile(`TextLayoutManager::measure|RCTTextLayoutManager measure|ParagraphShadowNode::measure|boundingRect|measureContent`)},
	{"skia", regexp.MustCompile(`(?i)RNSkia|SkiaView|SkCanvas|Sk[A-Z][a-z]+::|RNSkView|skia`)},
	{"svg", regexp.MustCompile(`RNSVG`)},
	{"flashlist", regexp.MustCompile(`FlashList|RecyclerView|AutoLayout`)},
	{"mount:create", regexp.MustCompile(`createComponentViewWithComponentHandle|RCTComponentViewRegistry dequeue|ComponentViewFactory|initWithFrame`)},
	{"mount:props", regexp.MustCompile(`updateProps:oldProps|updateProps:|PropsParser|Props::Props`)},
	{"mount:layout", regexp.MustCompile(`updateLayoutMetrics`)},
	{"mount:children", regexp.MustCompile(`mountChildComponentView|unmountChildComponentView`)},
	{"mount:finalize", regexp.MustCompile(`finalizeUpdates|prepareForRecycle|enqueueComponentViewWithComponentHandle`)},
	{"mount:other", regexp.MustCompile(`RCTMountingManager|MountingCoordinator|TelemetryController::pullTransaction`)},
	{"yoga", regexp.MustCompile(`yoga::|YGNode|calculateLayout`)},
	{"shadowtree-commit", regexp.MustCompile(`ShadowTree::commit|ShadowTree::tryCommit|cloneTree|cloneShadowTreeWithNewProps`)},
	{"reanimated-commit", regexp.MustCompile(`ReanimatedCommitHook|ReanimatedMountHook|performOperations|LayoutAnimationsProxy|LayoutAnimations`)},
	{"reanimated-frame", regexp.MustCompile(`AnimationFrameBatchinator|WorkletRuntime::runSync|worklets::`)},
	{"ca-commit", regexp.MustCompile(`CA::Transaction::commit|CA::Context::commit_transaction`)},
	{"ca-layout", regexp.MustCompile(`layout_and_display_if_needed|layoutSublayers|_layoutSubviews|layoutSubviews`)},
	{"ca-display", regexp.MustCompile(`CA::Layer::display|drawInContext|displayLayer`)},
	{"hermes-gc", regexp.MustCompile(`HadesGC|GCBase`)},
	{"hermes", regexp.MustCompile(`hermes::vm::Interpreter|hermes::vm::Runtime::interpret`)},
	{"react-scheduler", regexp.MustCompile(`RuntimeScheduler`)},
	{"ui-touch", regexp.MustCompile(`UIGestureRecognizer|_UIGestureEnvironment|sendEvent|RNGestureHandler|RCTSurfaceTouchHandler`)},
	{"menu", regexp.MustCompile(`UIContextMenu|_UIContextMenu|UIMenu|MenuView`)},
	{"runloop-wait", regexp.MustCompile(`mach_msg2_trap|__CFRunLoopServiceMachPort`)},
}

// Default thread name prefixes a profile covers: the main thread and React
// Native's JS thread.
var DefaultProfileThreads = []string{"Main Thread", "com.facebook.react.runtime.JavaScript"}

// ProfileOptions tunes ReadProfile.
type ProfileOptions struct {
	// PID keeps one process (the TOC's target); 0 keeps every process.
	PID int
	// Threads are thread-name prefixes (default DefaultProfileThreads).
	Threads []string
	// Windows are trace-relative spans; empty means the whole recording.
	Windows []Step
	// Classify buckets Running samples by FrameFamilies; Stacks lists the
	// heaviest leaf frames. Neither keeps only the per-thread totals.
	Classify bool
	Stacks   bool
	// Depth is how many families a classification key chains (default 2).
	Depth int
	// Top caps each ranked list (default 12).
	Top int
}

// Weighted is one ranked key with its sampled milliseconds.
type Weighted struct {
	Key string  `json:"key"`
	Ms  float64 `json:"ms"`
}

// ThreadProfile is one thread inside one window.
type ThreadProfile struct {
	Thread    string     `json:"thread"`
	RunningMs float64    `json:"runningMs"`
	Classes   []Weighted `json:"classes,omitempty"`
	Leaves    []Weighted `json:"leaves,omitempty"`
}

// WindowProfile is the profile of one window.
type WindowProfile struct {
	Label   string          `json:"label"`
	From    float64         `json:"from"`
	To      float64         `json:"to"`
	Threads []ThreadProfile `json:"threads"`
}

type threadAcc struct {
	running float64
	classes map[string]float64
	leaves  map[string]float64
}

// ThreadName trims Instruments' "Main Thread (0x571a60) (FixIt, pid: 11077)"
// to "Main Thread".
func ThreadName(label string) string {
	if i := strings.Index(label, " (0x"); i >= 0 {
		return label[:i]
	}
	return label
}

// ReadProfile aggregates the Running samples of a time-profile export per
// window and thread.
func ReadProfile(r io.Reader, opts ProfileOptions) ([]WindowProfile, error) {
	if len(opts.Threads) == 0 {
		opts.Threads = DefaultProfileThreads
	}
	if opts.Depth <= 0 {
		opts.Depth = 2
	}
	if opts.Top <= 0 {
		opts.Top = 12
	}
	windows := opts.Windows
	if len(windows) == 0 {
		windows = []Step{{Label: "recording", From: 0, To: math.Inf(1)}}
	}
	accs := make([]map[string]*threadAcc, len(windows))
	for i := range accs {
		accs[i] = map[string]*threadAcc{}
	}
	_, err := ReadTable(r, func(row Row) error {
		if row.Cell("thread-state").Label() != "Running" {
			return nil
		}
		thread := row.Cell("thread")
		if opts.PID > 0 && processPID(row.Cell("process"), thread) != opts.PID {
			return nil
		}
		name := ThreadName(thread.Label())
		if !hasPrefix(name, opts.Threads) {
			return nil
		}
		at, ok := row.Cell("time").Int()
		if !ok {
			return nil
		}
		t := float64(at) / 1e9
		w := 1.0
		if wv, ok := row.Cell("weight").Int(); ok {
			w = float64(wv) / 1e6
		}
		var frames []string
		if opts.Classify || opts.Stacks {
			frames = stackFrames(row.Cell("stack"))
		}
		for i, win := range windows {
			if t < win.From || t >= win.To {
				continue
			}
			acc := accs[i][name]
			if acc == nil {
				acc = &threadAcc{classes: map[string]float64{}, leaves: map[string]float64{}}
				accs[i][name] = acc
			}
			acc.running += w
			if opts.Classify {
				acc.classes[classifyStack(frames, opts.Depth)] += w
			}
			if opts.Stacks && len(frames) > 0 {
				acc.leaves[truncate(frames[0], 140)] += w
			}
		}
		return nil
	})
	if err != nil {
		return nil, diag(DiagTraceUnreadable, fmt.Sprintf("%s: %v", TableTimeProfile, err), reRecordFix)
	}
	out := make([]WindowProfile, len(windows))
	for i, win := range windows {
		wp := WindowProfile{Label: win.Label, From: win.From, To: win.To, Threads: []ThreadProfile{}}
		if math.IsInf(wp.To, 1) {
			wp.To = 0
		}
		for name, acc := range accs[i] {
			tp := ThreadProfile{Thread: name, RunningMs: round2(acc.running)}
			if opts.Classify {
				tp.Classes = rank(acc.classes, opts.Top)
			}
			if opts.Stacks {
				tp.Leaves = rank(acc.leaves, opts.Top)
			}
			wp.Threads = append(wp.Threads, tp)
		}
		sort.Slice(wp.Threads, func(a, b int) bool { return wp.Threads[a].RunningMs > wp.Threads[b].RunningMs })
		out[i] = wp
	}
	return out, nil
}

// classifyStack walks the frames leaf to root and chains the first depth
// families met (deepest first), printed root-most first: "ui-touch > menu".
// A stack matching no family is keyed by its leaf.
func classifyStack(frames []string, depth int) string {
	var cats []string
	for _, f := range frames {
		for _, fam := range FrameFamilies {
			if fam.Re.MatchString(f) {
				if !contains(cats, fam.Name) {
					cats = append(cats, fam.Name)
				}
				break
			}
		}
		if len(cats) >= depth {
			break
		}
	}
	if len(cats) == 0 {
		leaf := "?"
		if len(frames) > 0 {
			leaf = truncate(frames[0], 60)
		}
		return "(leaf) " + leaf
	}
	for i, j := 0, len(cats)-1; i < j; i, j = i+1, j-1 {
		cats[i], cats[j] = cats[j], cats[i]
	}
	return strings.Join(cats, " > ")
}

// stackFrames lists a backtrace cell's frame names, leaf first.
func stackFrames(stack *Node) []string {
	if stack == nil {
		return nil
	}
	bt := stack.Child("backtrace")
	if bt == nil {
		bt = stack
	}
	var out []string
	for _, f := range bt.Children {
		if f.Name != "frame" {
			continue
		}
		name := f.Attrs["name"]
		if name == "" {
			name = f.Attrs["addr"]
		}
		out = append(out, name)
	}
	return out
}

var pidFmtRe = regexp.MustCompile(`\((\d+)\)$`)

func processPID(proc, thread *Node) int {
	for _, p := range []*Node{proc, thread.Child("process")} {
		if p == nil {
			continue
		}
		if v, ok := p.Child("pid").Int(); ok {
			return int(v)
		}
		if m := pidFmtRe.FindStringSubmatch(p.Label()); m != nil {
			v, _ := strconv.Atoi(m[1])
			return v
		}
	}
	return 0
}

func rank(m map[string]float64, top int) []Weighted {
	out := make([]Weighted, 0, len(m))
	for k, v := range m {
		out = append(out, Weighted{Key: k, Ms: round2(v)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Ms != out[j].Ms {
			return out[i].Ms > out[j].Ms
		}
		return out[i].Key < out[j].Key
	})
	if len(out) > top {
		out = out[:top]
	}
	return out
}

func hasPrefix(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// ParseWindow reads a `--window a-b` span in trace seconds ("12.5-20").
func ParseWindow(spec string) (Step, error) {
	from, to, ok := strings.Cut(spec, "-")
	a, errA := strconv.ParseFloat(strings.TrimSpace(from), 64)
	b, errB := strconv.ParseFloat(strings.TrimSpace(to), 64)
	if !ok || errA != nil || errB != nil || a < 0 || b <= a {
		return Step{}, diag(DiagUsage, fmt.Sprintf("--window %q is not <from>-<to> seconds of the recording", spec),
			"perflab analyze <trace> --window 12.5-20 --classify")
	}
	return Step{Label: spec, From: a, To: b}, nil
}

package framestats

import (
	"sort"
	"strconv"
	"strings"
)

// Perfetto field numbers (perfetto/protos, stable across releases):
// Trace.packet=1; TracePacket.ftrace_events=1, process_tree=2,
// timestamp=8, frame_timeline_event=76; FtraceEventBundle.cpu=1, event=2;
// FtraceEvent.timestamp=1, pid=2, print=3, sched_switch=4;
// PrintFtraceEvent.buf=2; SchedSwitchFtraceEvent prev_comm=1 prev_pid=2
// next_comm=5 next_pid=6; ProcessTree.processes=1 (Process pid=1,
// cmdline=3), threads=2 (Thread tid=1 name=2 tgid=5); FrameTimelineEvent
// expected_display=1, actual_display=2, expected_surface=3,
// actual_surface=4, frame_end=5.
const (
	fTracePacket     = 1
	fFtraceBundle    = 1
	fProcessTree     = 2
	fPacketTimestamp = 8
	fFrameTimeline   = 76
)

// printEvent is one atrace marker written through ftrace print.
type printEvent struct {
	ts  int64
	tid int
	buf string
}

// timelineKind is a FrameTimelineEvent oneof member.
type timelineKind int

const (
	tlExpectedDisplay timelineKind = 1
	tlActualDisplay   timelineKind = 2
	tlExpectedSurface timelineKind = 3
	tlActualSurface   timelineKind = 4
	tlFrameEnd        timelineKind = 5
)

// timelineEvent is one FrameTimeline packet, flattened.
type timelineEvent struct {
	ts           int64
	kind         timelineKind
	cookie       int64
	token        int64
	displayToken int64
	pid          int
	layer        string
	presentType  int
	jankType     int
}

// rawTrace is what one walk over the packets collects.
type rawTrace struct {
	packets  int
	prints   []printEvent
	comms    map[int]string // tid -> comm, from sched_switch
	cmdlines map[int]string // pid -> cmdline[0], from process_tree
	timeline []timelineEvent
}

func decodeTrace(raw []byte) rawTrace {
	t := rawTrace{comms: map[int]string{}, cmdlines: map[int]string{}}
	for _, pk := range fields(raw) {
		if pk.num != fTracePacket || pk.wt != 2 {
			continue
		}
		t.packets++
		pfs := fields(pk.data)
		var ts int64
		for _, f := range pfs {
			if f.num == fPacketTimestamp && f.wt == 0 {
				ts = int64(f.u)
			}
		}
		for _, f := range pfs {
			if f.wt != 2 {
				continue
			}
			switch f.num {
			case fFtraceBundle:
				t.decodeFtrace(f.data)
			case fProcessTree:
				t.decodeProcessTree(f.data)
			case fFrameTimeline:
				t.decodeTimeline(ts, f.data)
			}
		}
	}
	// Bundles arrive per CPU; slices need each thread's markers in order.
	sort.SliceStable(t.prints, func(i, j int) bool { return t.prints[i].ts < t.prints[j].ts })
	sort.SliceStable(t.timeline, func(i, j int) bool { return t.timeline[i].ts < t.timeline[j].ts })
	return t
}

func (t *rawTrace) decodeFtrace(bundle []byte) {
	for _, bf := range fields(bundle) {
		if bf.num != 2 || bf.wt != 2 {
			continue
		}
		var ets int64
		var pid int
		efs := fields(bf.data)
		for _, ef := range efs {
			if ef.wt == 0 && ef.num == 1 {
				ets = int64(ef.u)
			}
			if ef.wt == 0 && ef.num == 2 {
				pid = int(ef.u)
			}
		}
		for _, ef := range efs {
			if ef.wt != 2 {
				continue
			}
			switch ef.num {
			case 3: // print
				for _, pf := range fields(ef.data) {
					if pf.num == 2 && pf.wt == 2 {
						t.prints = append(t.prints, printEvent{ts: ets, tid: pid, buf: strings.TrimRight(string(pf.data), "\n")})
					}
				}
			case 4: // sched_switch
				var prevPid, nextPid int
				var prevComm, nextComm string
				for _, sf := range fields(ef.data) {
					switch sf.num {
					case 1:
						prevComm = string(sf.data)
					case 2:
						prevPid = int(sf.u)
					case 5:
						nextComm = string(sf.data)
					case 6:
						nextPid = int(sf.u)
					}
				}
				if prevComm != "" {
					t.comms[prevPid] = prevComm
				}
				if nextComm != "" {
					t.comms[nextPid] = nextComm
				}
			}
		}
	}
}

func (t *rawTrace) decodeProcessTree(tree []byte) {
	for _, f := range fields(tree) {
		if f.wt != 2 {
			continue
		}
		switch f.num {
		case 1: // Process
			var pid int
			cmdline := ""
			for _, pf := range fields(f.data) {
				if pf.num == 1 && pf.wt == 0 {
					pid = int(pf.u)
				}
				if pf.num == 3 && pf.wt == 2 && cmdline == "" {
					cmdline = string(pf.data)
				}
			}
			if pid > 0 && cmdline != "" {
				t.cmdlines[pid] = cmdline
			}
		case 2: // Thread
			var tid int
			name := ""
			for _, tf := range fields(f.data) {
				if tf.num == 1 && tf.wt == 0 {
					tid = int(tf.u)
				}
				if tf.num == 2 && tf.wt == 2 {
					name = string(tf.data)
				}
			}
			if tid > 0 && name != "" {
				if _, known := t.comms[tid]; !known {
					t.comms[tid] = name
				}
			}
		}
	}
}

func (t *rawTrace) decodeTimeline(ts int64, event []byte) {
	for _, ev := range fields(event) {
		if ev.wt != 2 || ev.num < 1 || ev.num > 5 {
			continue
		}
		e := timelineEvent{ts: ts, kind: timelineKind(ev.num)}
		for _, x := range fields(ev.data) {
			switch x.num {
			case 1:
				e.cookie = int64(x.u)
			case 2:
				e.token = int64(x.u)
			}
			switch e.kind {
			case tlExpectedSurface, tlActualSurface:
				switch x.num {
				case 3:
					e.displayToken = int64(x.u)
				case 4:
					e.pid = int(x.u)
				case 5:
					e.layer = string(x.data)
				case 6:
					e.presentType = int(x.u)
				case 9:
					e.jankType = int(x.u)
				}
			case tlExpectedDisplay, tlActualDisplay:
				switch x.num {
				case 3:
					e.pid = int(x.u)
				case 4:
					e.presentType = int(x.u)
				case 7:
					e.jankType = int(x.u)
				}
			}
		}
		t.timeline = append(t.timeline, e)
	}
}

// slice is one completed atrace B/E pair on a thread.
type slice struct {
	tid   int
	tgid  int
	name  string
	start int64
	end   int64
	depth int
}

// buildSlices pairs every thread's B/E markers into slices, keeping those
// begun by process pid. An E closes the innermost open slice of its writer
// thread whatever pid it names; unclosed slices at the end are dropped.
func buildSlices(prints []printEvent, pid int) []slice {
	type open struct {
		tgid  int
		name  string
		start int64
	}
	stacks := map[int][]open{}
	var out []slice
	for _, p := range prints {
		switch {
		case strings.HasPrefix(p.buf, "B|"):
			rest := p.buf[2:]
			bar := strings.IndexByte(rest, '|')
			if bar < 0 {
				continue
			}
			tgid, err := strconv.Atoi(rest[:bar])
			if err != nil {
				continue
			}
			stacks[p.tid] = append(stacks[p.tid], open{tgid: tgid, name: rest[bar+1:], start: p.ts})
		case p.buf == "E" || strings.HasPrefix(p.buf, "E|"):
			stack := stacks[p.tid]
			if len(stack) == 0 {
				continue
			}
			top := stack[len(stack)-1]
			stacks[p.tid] = stack[:len(stack)-1]
			if top.tgid == pid {
				out = append(out, slice{tid: p.tid, tgid: top.tgid, name: top.name, start: top.start, end: p.ts, depth: len(stack) - 1})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].start < out[j].start })
	return out
}

// tailComm is the kernel comm of a process's main thread: the last 15
// bytes of its name (TASK_COMM_LEN 16 with the terminator).
func tailComm(name string) string {
	if len(name) <= 15 {
		return name
	}
	return name[len(name)-15:]
}

// resolvePID finds the package's pid: process_tree cmdline first, then the
// FrameTimeline layer names (`<pkg>/<activity>...`), then a main thread whose
// comm is the package tail and which writes markers under its own tid.
func resolvePID(t rawTrace, pkg string) (int, string) {
	best := 0
	for pid, cmdline := range t.cmdlines {
		if cmdline == pkg && (best == 0 || pid < best) {
			best = pid
		}
	}
	if best > 0 {
		return best, "process_tree"
	}
	votes := map[int]int{}
	for _, e := range t.timeline {
		if (e.kind == tlActualSurface || e.kind == tlExpectedSurface) && e.pid > 0 &&
			strings.Contains(e.layer, pkg+"/") {
			votes[e.pid]++
		}
	}
	for pid, n := range votes {
		if best == 0 || n > votes[best] || (n == votes[best] && pid < best) {
			best = pid
		}
	}
	if best > 0 {
		return best, "frametimeline"
	}
	tail := tailComm(pkg)
	for _, p := range t.prints {
		if !strings.HasPrefix(p.buf, "B|"+strconv.Itoa(p.tid)+"|") {
			continue
		}
		if t.comms[p.tid] == tail {
			return p.tid, "sched_comm"
		}
	}
	return 0, ""
}

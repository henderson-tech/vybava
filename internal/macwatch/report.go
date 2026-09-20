package macwatch

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// ReportOptions shape Summarize.
type ReportOptions struct {
	Top          int     // offenders per ranking (default 15)
	Cores        int     // load1 >= SpikeFactor*Cores is a spike (default 1)
	SpikeFactor  float64 // default 2
	TimelineRows int     // at most this many timeline points, peaks always kept (default 24)
}

// Report is the summarized file: stable for --json, rendered by WriteText
// and WriteMarkdown.
type Report struct {
	Samples         int             `json:"samples"`
	From            time.Time       `json:"from"`
	To              time.Time       `json:"to"`
	IntervalSeconds float64         `json:"intervalSeconds"`
	Cores           int             `json:"cores"`
	SpikeLoad       float64         `json:"spikeLoad"`
	Done            bool            `json:"done"`
	Timeline        []TimelinePoint `json:"timeline"`
	PeakLoad        TimelinePoint   `json:"peakLoad"`
	MinFree         TimelinePoint   `json:"minFree"`
	ByCPU           []Offender      `json:"byCpu"`
	ByRSS           []Offender      `json:"byRss"`
	Projects        []Aggregate     `json:"projects"`
	Owners          []Aggregate     `json:"owners"`
	Spikes          []Spike         `json:"spikes"`
}

// TimelinePoint is one system row reduced to what the timeline shows.
type TimelinePoint struct {
	At           time.Time `json:"at"`
	Load1        float64   `json:"load1"`
	Load5        float64   `json:"load5"`
	FreeGB       float64   `json:"freeGb"`
	CompressorGB float64   `json:"compressorGb"`
	Claude       int       `json:"claude"`
	Sims         int       `json:"sims"`
	PeakLoad     bool      `json:"peakLoad,omitempty"`
	MinFree      bool      `json:"minFree,omitempty"`
}

// Offender is one process (pid + executable) integrated over the file.
type Offender struct {
	PID            int       `json:"pid"`
	Cmd            string    `json:"cmd"`
	Project        string    `json:"project"`
	Cwd            string    `json:"cwd"`
	Owner          string    `json:"owner"`
	CPUCoreMinutes float64   `json:"cpuCoreMinutes"`
	PeakCPU        float64   `json:"peakCpu"`
	PeakRSSMB      int       `json:"peakRssMb"`
	Samples        int       `json:"samples"`
	FirstSeen      time.Time `json:"firstSeen"`
	LastSeen       time.Time `json:"lastSeen"`
}

// Aggregate is a project or an owner session summed over its processes.
type Aggregate struct {
	Name           string  `json:"name"`
	CPUCoreMinutes float64 `json:"cpuCoreMinutes"`
	PeakRSSMB      int     `json:"peakRssMb"` // largest sum of RSS across its processes in one sample
	Processes      int     `json:"processes"`
}

// Spike is one sample whose load1 crossed the spike line, with the three
// processes that were burning the most CPU at that moment.
type Spike struct {
	At    time.Time    `json:"at"`
	Load1 float64      `json:"load1"`
	Top   []ProcessRow `json:"top"`
}

type offenderKey struct {
	pid int
	exe string
}

// Summarize integrates a parsed file. CPU is integrated as cpu% x the
// interval to the next sample (the last sample gets the median interval),
// expressed in core-minutes.
func Summarize(file File, opts ReportOptions) Report {
	if opts.Top <= 0 {
		opts.Top = 15
	}
	if opts.Cores <= 0 {
		opts.Cores = 1
	}
	if opts.SpikeFactor <= 0 {
		opts.SpikeFactor = 2
	}
	if opts.TimelineRows <= 0 {
		opts.TimelineRows = 24
	}
	samples := file.Samples
	report := Report{Samples: len(samples), Cores: opts.Cores, SpikeLoad: opts.SpikeFactor * float64(opts.Cores), Done: file.Done}
	if len(samples) == 0 {
		return report
	}
	report.From, report.To = samples[0].System.At, samples[len(samples)-1].System.At
	intervals := intervalsOf(samples)
	report.IntervalSeconds = median(intervals)

	offenders := map[offenderKey]*Offender{}
	projects := map[string]*Aggregate{}
	owners := map[string]*Aggregate{}
	projectProcs := map[string]map[offenderKey]struct{}{}
	ownerProcs := map[string]map[offenderKey]struct{}{}
	peakIdx, minFreeIdx := 0, 0
	for i, s := range samples {
		if s.System.Load1 > samples[peakIdx].System.Load1 {
			peakIdx = i
		}
		if s.System.FreeGB < samples[minFreeIdx].System.FreeGB {
			minFreeIdx = i
		}
		interval := intervals[i]
		if interval <= 0 {
			interval = report.IntervalSeconds
		}
		projectRSS := map[string]int{}
		ownerRSS := map[string]int{}
		for _, p := range s.Processes {
			key := offenderKey{pid: p.PID, exe: exeOf(p.Cmd)}
			minutes := p.CPU / 100 * interval / 60
			o := offenders[key]
			if o == nil {
				o = &Offender{PID: p.PID, Cmd: p.Cmd, Project: p.Project, Cwd: p.Cwd, Owner: p.Owner(), FirstSeen: p.At}
				offenders[key] = o
			}
			o.CPUCoreMinutes += minutes
			o.PeakCPU = max(o.PeakCPU, p.CPU)
			o.PeakRSSMB = max(o.PeakRSSMB, p.RSSMB)
			o.Samples++
			o.LastSeen = p.At
			if o.Project == "-" && p.Project != "-" {
				o.Project, o.Cwd = p.Project, p.Cwd
			}

			accumulate(projects, projectProcs, p.Project, key, minutes)
			accumulate(owners, ownerProcs, p.Owner(), key, minutes)
			projectRSS[p.Project] += p.RSSMB
			ownerRSS[p.Owner()] += p.RSSMB
		}
		for name, rss := range projectRSS {
			projects[name].PeakRSSMB = max(projects[name].PeakRSSMB, rss)
		}
		for name, rss := range ownerRSS {
			owners[name].PeakRSSMB = max(owners[name].PeakRSSMB, rss)
		}
		if s.System.Load1 >= report.SpikeLoad {
			top := append([]ProcessRow(nil), s.Processes...)
			sort.SliceStable(top, func(a, b int) bool { return top[a].CPU > top[b].CPU })
			if len(top) > 3 {
				top = top[:3]
			}
			report.Spikes = append(report.Spikes, Spike{At: s.System.At, Load1: s.System.Load1, Top: top})
		}
	}

	report.Timeline = timeline(samples, opts.TimelineRows, peakIdx, minFreeIdx)
	report.PeakLoad = point(samples[peakIdx].System)
	report.MinFree = point(samples[minFreeIdx].System)

	all := make([]Offender, 0, len(offenders))
	for _, o := range offenders {
		all = append(all, *o)
	}
	report.ByCPU = rank(all, opts.Top, func(a, b Offender) bool {
		if a.CPUCoreMinutes != b.CPUCoreMinutes {
			return a.CPUCoreMinutes > b.CPUCoreMinutes
		}
		return a.PID < b.PID
	})
	report.ByRSS = rank(all, opts.Top, func(a, b Offender) bool {
		if a.PeakRSSMB != b.PeakRSSMB {
			return a.PeakRSSMB > b.PeakRSSMB
		}
		return a.PID < b.PID
	})
	report.Projects = aggregates(projects, projectProcs)
	report.Owners = aggregates(owners, ownerProcs)
	return report
}

func accumulate(agg map[string]*Aggregate, procs map[string]map[offenderKey]struct{}, name string, key offenderKey, minutes float64) {
	a := agg[name]
	if a == nil {
		a = &Aggregate{Name: name}
		agg[name] = a
		procs[name] = map[offenderKey]struct{}{}
	}
	a.CPUCoreMinutes += minutes
	procs[name][key] = struct{}{}
}

func aggregates(agg map[string]*Aggregate, procs map[string]map[offenderKey]struct{}) []Aggregate {
	out := make([]Aggregate, 0, len(agg))
	for name, a := range agg {
		a.Processes = len(procs[name])
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CPUCoreMinutes != out[j].CPUCoreMinutes {
			return out[i].CPUCoreMinutes > out[j].CPUCoreMinutes
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func rank(all []Offender, top int, less func(a, b Offender) bool) []Offender {
	out := append([]Offender(nil), all...)
	sort.Slice(out, func(i, j int) bool { return less(out[i], out[j]) })
	if len(out) > top {
		out = out[:top]
	}
	return out
}

func intervalsOf(samples []Sample) []float64 {
	intervals := make([]float64, len(samples))
	for i := 0; i+1 < len(samples); i++ {
		intervals[i] = samples[i+1].System.At.Sub(samples[i].System.At).Seconds()
	}
	if len(samples) > 1 {
		intervals[len(samples)-1] = median(intervals[:len(samples)-1])
	} else {
		intervals[0] = 30
	}
	return intervals
}

func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	return sorted[len(sorted)/2]
}

func timeline(samples []Sample, rows, peakIdx, minFreeIdx int) []TimelinePoint {
	keep := map[int]struct{}{peakIdx: {}, minFreeIdx: {}, 0: {}, len(samples) - 1: {}}
	if len(samples) <= rows {
		for i := range samples {
			keep[i] = struct{}{}
		}
	} else {
		for k := 0; k < rows; k++ {
			keep[k*(len(samples)-1)/(rows-1)] = struct{}{}
		}
	}
	indexes := make([]int, 0, len(keep))
	for i := range keep {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	out := make([]TimelinePoint, 0, len(indexes))
	for _, i := range indexes {
		p := point(samples[i].System)
		p.PeakLoad, p.MinFree = i == peakIdx, i == minFreeIdx
		out = append(out, p)
	}
	return out
}

func point(s SystemRow) TimelinePoint {
	return TimelinePoint{At: s.At, Load1: s.Load1, Load5: s.Load5, FreeGB: s.FreeGB, CompressorGB: s.CompressorGB, Claude: s.Claude, Sims: s.Sims}
}

func exeOf(cmd string) string {
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// WriteText renders the report for a terminal.
func WriteText(w io.Writer, r Report) error {
	return render(w, r, false)
}

// WriteMarkdown renders the report as Markdown tables (vitrinka artifacts).
func WriteMarkdown(w io.Writer, r Report) error {
	return render(w, r, true)
}

func render(w io.Writer, r Report, md bool) error {
	p := &printer{w: w, md: md}
	p.heading(1, fmt.Sprintf("macwatch %s to %s", r.From.Format(TimeLayout), r.To.Format(TimeLayout)))
	trailer := "run still open (no DONE trailer)"
	if r.Done {
		trailer = "run complete"
	}
	p.line(fmt.Sprintf("%d samples, median interval %.0fs, %d cores, spike line load1 >= %.0f, %s.", r.Samples, r.IntervalSeconds, r.Cores, r.SpikeLoad, trailer))
	if r.Samples == 0 {
		return p.err
	}
	p.line(fmt.Sprintf("Peak load1 %.2f at %s; least free memory %.2f GB at %s.", r.PeakLoad.Load1, r.PeakLoad.At.Format("15:04:05"), r.MinFree.FreeGB, r.MinFree.At.Format("15:04:05")))

	p.heading(2, "Timeline")
	t := table{header: []string{"time", "load1", "load5", "free GB", "compressor GB", "claude", "sims", ""}}
	for _, pt := range r.Timeline {
		mark := ""
		if pt.PeakLoad {
			mark += "peak load "
		}
		if pt.MinFree {
			mark += "min free"
		}
		t.add(pt.At.Format("15:04:05"), f2(pt.Load1), f2(pt.Load5), f2(pt.FreeGB), f2(pt.CompressorGB), itoa(pt.Claude), itoa(pt.Sims), strings.TrimSpace(mark))
	}
	p.table(t)

	p.heading(2, "Top offenders by integrated CPU (core-minutes)")
	p.table(offenderTable(r.ByCPU))
	p.heading(2, "Top offenders by peak RSS")
	p.table(offenderTable(r.ByRSS))

	p.heading(2, "Per project")
	t = table{header: []string{"project", "core-min", "peak RSS MB", "processes"}}
	for _, a := range r.Projects {
		t.add(a.Name, f1(a.CPUCoreMinutes), itoa(a.PeakRSSMB), itoa(a.Processes))
	}
	p.table(t)

	p.heading(2, "Per owner session")
	t = table{header: []string{"owner", "core-min", "peak RSS MB", "processes"}}
	for _, a := range r.Owners {
		t.add(a.Name, f1(a.CPUCoreMinutes), itoa(a.PeakRSSMB), itoa(a.Processes))
	}
	p.table(t)

	p.heading(2, fmt.Sprintf("Spike ledger (load1 >= %.0f)", r.SpikeLoad))
	if len(r.Spikes) == 0 {
		p.line("No spikes.")
		return p.err
	}
	t = table{header: []string{"time", "load1", "#", "pid", "cpu %", "RSS MB", "project", "owner", "cmd"}}
	for _, s := range r.Spikes {
		for i, top := range s.Top {
			at, load := "", ""
			if i == 0 {
				at, load = s.At.Format("15:04:05"), f2(s.Load1)
			}
			t.add(at, load, itoa(i+1), itoa(top.PID), f1(top.CPU), itoa(top.RSSMB), top.Project, top.Owner(), top.Cmd)
		}
	}
	p.table(t)
	return p.err
}

func offenderTable(rows []Offender) table {
	t := table{header: []string{"pid", "core-min", "peak cpu %", "peak RSS MB", "seen", "first", "last", "project", "owner", "cwd", "cmd"}}
	for _, o := range rows {
		t.add(itoa(o.PID), f1(o.CPUCoreMinutes), f1(o.PeakCPU), itoa(o.PeakRSSMB), itoa(o.Samples),
			o.FirstSeen.Format("15:04:05"), o.LastSeen.Format("15:04:05"), o.Project, o.Owner, o.Cwd, o.Cmd)
	}
	return t
}

type table struct {
	header []string
	rows   [][]string
}

func (t *table) add(cells ...string) { t.rows = append(t.rows, cells) }

type printer struct {
	w   io.Writer
	md  bool
	err error
}

func (p *printer) line(s string) {
	if p.err == nil {
		_, p.err = fmt.Fprintln(p.w, s)
	}
}

func (p *printer) heading(level int, s string) {
	if p.md {
		p.line("")
		p.line(strings.Repeat("#", level) + " " + s)
		p.line("")
		return
	}
	p.line("")
	p.line(s)
	if level == 1 {
		p.line(strings.Repeat("=", len(s)))
	} else {
		p.line(strings.Repeat("-", len(s)))
	}
}

func (p *printer) table(t table) {
	if len(t.rows) == 0 {
		p.line("(none)")
		return
	}
	if p.md {
		p.line("| " + strings.Join(escapeCells(t.header), " | ") + " |")
		p.line("|" + strings.Repeat(" --- |", len(t.header)))
		for _, row := range t.rows {
			p.line("| " + strings.Join(escapeCells(row), " | ") + " |")
		}
		return
	}
	widths := make([]int, len(t.header))
	for i, h := range t.header {
		widths[i] = len([]rune(h))
	}
	for _, row := range t.rows {
		for i, c := range row {
			if i < len(widths) {
				widths[i] = max(widths[i], len([]rune(c)))
			}
		}
	}
	format := func(cells []string) string {
		var b strings.Builder
		for i, c := range cells {
			if i > 0 {
				b.WriteString("  ")
			}
			b.WriteString(c)
			if i < len(cells)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-len([]rune(c))))
			}
		}
		return strings.TrimRight(b.String(), " ")
	}
	p.line(format(t.header))
	for _, row := range t.rows {
		p.line(format(row))
	}
}

func escapeCells(cells []string) []string {
	out := make([]string, len(cells))
	for i, c := range cells {
		out[i] = strings.ReplaceAll(c, "|", "\\|")
	}
	return out
}

func f1(v float64) string { return fmt.Sprintf("%.1f", v) }
func f2(v float64) string { return fmt.Sprintf("%.2f", v) }
func itoa(v int) string   { return fmt.Sprintf("%d", v) }

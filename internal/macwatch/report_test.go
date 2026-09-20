package macwatch

import (
	"bytes"
	"strings"
	"testing"
)

// prototypeTSV is what the bash sampler wrote: unevaluated "2613/1024"
// megabyte cells, "-" owners, a DONE trailer.
const prototypeTSV = `S	2026-09-20T13:00:00	10	11	12	2000	9	15000	93	2613/1024	42	55	3	4	1	2	0	2
P	2026-09-20T13:00:00	300	200	100	1800	00:23	??	claude	100	ttys004	FixIt-Technologies/FixIt:/macwatch	/Users/me/Work/Projects/FixIt-Technologies/FixIt/.worktrees/macwatch	node processChild.js
P	2026-09-20T13:00:00	400	1	50	500	03-04:23:36	??	-	-	-	-	/	WindowServer -daemon
S	2026-09-20T13:00:30	30	11	12	2000	9	15000	93	1.5	42	55	3	4	1	2	0	2
P	2026-09-20T13:00:30	300	200	50	2000	00:53	??	claude	100	ttys004	FixIt-Technologies/FixIt:macwatch	/Users/me/Work/Projects/FixIt-Technologies/FixIt/.worktrees/macwatch	node processChild.js
P	2026-09-20T13:00:30	400	1	50	500	03-04:24:06	??	-	-	-	-	/	WindowServer -daemon
P	2026-09-20T13:00:30	900	100	20	8000	00:01	??	claude	100	ttys004	FixIt-Technologies/FixIt:macwatch	/Users/me/Work/Projects/FixIt-Technologies/FixIt/.worktrees/macwatch	java -jar gradle.jar
S	2026-09-20T13:01:00	5	11	12	2000	9	15000	93	4	42	55	3	4	1	2	0	2
P	2026-09-20T13:01:00	400	1	50	500	03-04:24:36	??	-	-	-	-	/	WindowServer -daemon
DONE 2026-09-20T13:01:00 3 samples
`

func TestReadPrototypeFile(t *testing.T) {
	file, err := Read(strings.NewReader(prototypeTSV))
	if err != nil {
		t.Fatal(err)
	}
	if len(file.Samples) != 3 || !file.Done {
		t.Fatalf("samples=%d done=%v", len(file.Samples), file.Done)
	}
	first := file.Samples[0]
	if got := first.System.FreeGB; got < 2.55 || got > 2.56 {
		t.Fatalf("megabyte division cell: %v", got)
	}
	if len(first.Processes) != 2 || first.Processes[1].OwnerKind != "-" || first.Processes[1].OwnerPID != 0 || first.Processes[1].Owner() != "-" {
		t.Fatalf("unowned row: %+v", first.Processes)
	}
	if first.Processes[0].Owner() != "claude 100 (ttys004)" {
		t.Fatalf("owner label: %q", first.Processes[0].Owner())
	}
	// A row written by this applet reads back identically.
	if back := first.System.TSV(); !strings.HasPrefix(back, "S\t2026-09-20T13:00:00\t10\t11\t12\t2000\t9\t15000\t93\t2.55") {
		t.Fatalf("system round trip: %s", back)
	}
	if _, err := Read(strings.NewReader("S\t2026-09-20T13:00:00\tx\n")); err == nil {
		t.Fatal("a malformed known row must fail loudly")
	}
}

func TestSummarizeIntegratesAndFindsSpikes(t *testing.T) {
	file, err := Read(strings.NewReader(prototypeTSV))
	if err != nil {
		t.Fatal(err)
	}
	r := Summarize(file, ReportOptions{Top: 2, Cores: 10, TimelineRows: 2})
	if r.IntervalSeconds != 30 || r.SpikeLoad != 20 || !r.Done {
		t.Fatalf("header: %+v", r)
	}
	// jest: 100 % x 30 s + 50 % x 30 s = 45 core-seconds = 0.75 core-minutes.
	if len(r.ByCPU) != 2 || r.ByCPU[0].PID != 300 || r.ByCPU[0].CPUCoreMinutes != 0.75 || r.ByCPU[0].PeakCPU != 100 || r.ByCPU[0].Samples != 2 {
		t.Fatalf("by cpu: %+v", r.ByCPU)
	}
	// WindowServer: 50 % x 30 s x 3 samples (the last one gets the median interval).
	if r.ByCPU[1].PID != 400 || r.ByCPU[1].CPUCoreMinutes != 0.75 || r.ByCPU[1].Owner != "-" {
		t.Fatalf("by cpu #2: %+v", r.ByCPU[1])
	}
	if r.ByRSS[0].PID != 900 || r.ByRSS[0].PeakRSSMB != 8000 {
		t.Fatalf("by rss: %+v", r.ByRSS)
	}
	if len(r.Projects) != 2 || r.Projects[0].Name != "FixIt-Technologies/FixIt:macwatch" || r.Projects[0].Processes != 2 || r.Projects[0].PeakRSSMB != 10000 {
		t.Fatalf("projects: %+v", r.Projects)
	}
	if len(r.Owners) != 2 || r.Owners[0].Name != "claude 100 (ttys004)" || r.Owners[1].Name != "-" {
		t.Fatalf("owners: %+v", r.Owners)
	}
	if len(r.Spikes) != 1 || r.Spikes[0].Load1 != 30 || len(r.Spikes[0].Top) != 3 || r.Spikes[0].Top[0].PID != 300 {
		t.Fatalf("spikes: %+v", r.Spikes)
	}
	// Two rows requested, but the load peak (sample 2) is always kept.
	if len(r.Timeline) != 3 || !r.Timeline[1].PeakLoad || !r.Timeline[1].MinFree || r.PeakLoad.Load1 != 30 || r.MinFree.FreeGB != 1.5 {
		t.Fatalf("timeline: %+v", r.Timeline)
	}
}

func TestWriteMarkdownEscapesAndSections(t *testing.T) {
	file, _ := Read(strings.NewReader(strings.ReplaceAll(prototypeTSV, "gradle.jar", "a|b")))
	var md, text bytes.Buffer
	if err := WriteMarkdown(&md, Summarize(file, ReportOptions{Cores: 10})); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## Timeline", "| time | load1 |", "## Top offenders by integrated CPU", "## Per project", "## Per owner session", "## Spike ledger (load1 >= 20)", `a\|b`, "peak load"} {
		if !strings.Contains(md.String(), want) {
			t.Errorf("markdown lacks %q:\n%s", want, md.String())
		}
	}
	if err := WriteText(&text, Summarize(File{}, ReportOptions{})); err != nil || !strings.Contains(text.String(), "0 samples") || strings.Contains(text.String(), "|") {
		t.Fatalf("empty text report: err=%v\n%s", err, text.String())
	}
}

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/henderson-tech/vybava/internal/macwatch"
)

func TestMacwatchReportReadsSampledFile(t *testing.T) {
	runner := func(_ context.Context, name string, _ ...string) ([]byte, error) {
		switch name {
		case "sysctl":
			return []byte("{ 40.0 20.0 10.0 }"), nil
		case "top":
			return []byte("Processes: 10 total, 2 running, 8 sleeping, 100 threads\nPhysMem: 90G used (1G wired, 40G compressor), 6G unused.\n"), nil
		case "ps":
			return []byte("  300   1  97.8 1024000 00:23 ??  /usr/bin/node build.js\n"), nil
		case "lsof":
			return []byte("p300\nfcwd\nn/Users/me/Work/Projects/org/repo\n"), nil
		}
		return nil, nil
	}
	sampler := macwatch.Sampler{Run: runner, Home: "/Users/me"}
	out := filepath.Join(t.TempDir(), "load.tsv")

	var stdout, stderr bytes.Buffer
	rt := runtime{stdout: &stdout, stderr: &stderr}
	cmd := rt.macwatchCommandWithSampler("macwatch", sampler, 14)
	cmd.SetArgs([]string{"sample", "--out", out, "--every", "1s", "--for", "1ms"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(data), "S\t") || !strings.Contains(string(data), "\nP\t") || !strings.Contains(string(data), "\nDONE ") {
		t.Fatalf("sampled file:\n%s", data)
	}

	stdout.Reset()
	rt.json = true
	cmd = rt.macwatchCommandWithSampler("macwatch", sampler, 14)
	cmd.SetArgs([]string{"report", out})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var report macwatch.Report
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Samples != 1 || report.Cores != 14 || report.SpikeLoad != 28 || len(report.Spikes) != 1 || report.ByCPU[0].Project != "org/repo" || !report.Done {
		t.Fatalf("report: %+v", report)
	}

	stdout.Reset()
	rt.json = false
	cmd = rt.macwatchCommandWithSampler("macwatch", sampler, 14)
	cmd.SetArgs([]string{"report", out, "--md", "--cores", "100"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "| 300 |") || !strings.Contains(stdout.String(), "No spikes.") {
		t.Fatalf("markdown report:\n%s", stdout.String())
	}
}

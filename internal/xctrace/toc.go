package xctrace

import (
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"
)

// TOC is `xctrace export --toc`: one entry per recorded run.
type TOC struct {
	Runs []Run
}

// Device is the recorded device as the TOC names it. The device's own
// display name is personal and left out.
type Device struct {
	Platform string `json:"platform"`
	Model    string `json:"model"`
	OS       string `json:"os"`
	UDID     string `json:"udid"`
}

// Process is the run's target process.
type Process struct {
	Name string `json:"name"`
	PID  int    `json:"pid"`
}

// Table is one TOC table: its schema and the attributes that tell twin
// schemas apart (kdebug codes, tick frequency).
type Table struct {
	Schema string
	Attrs  map[string]string
}

// Run is one recorded run.
type Run struct {
	Number    int
	Device    Device
	Process   Process
	Start     time.Time
	End       time.Time
	DurationS float64
	EndReason string
	TimeLimit string
	Template  string
	Tables    []Table
}

// RecordingMs is how long the run recorded: the TOC <duration>, never the
// time limit (a window stopped early divides by what it recorded).
func (r Run) RecordingMs() float64 { return r.DurationS * 1000 }

// Schemas lists the run's table schemas once each, in TOC order.
func (r Run) Schemas() []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range r.Tables {
		if !seen[t.Schema] {
			seen[t.Schema] = true
			out = append(out, t.Schema)
		}
	}
	return out
}

// Has reports whether the run lists a table with this schema.
func (r Run) Has(schema string) bool {
	for _, t := range r.Tables {
		if t.Schema == schema {
			return true
		}
	}
	return false
}

type tocXML struct {
	Runs []struct {
		Number string `xml:"number,attr"`
		Info   struct {
			Target struct {
				Device struct {
					Platform string `xml:"platform,attr"`
					Model    string `xml:"model,attr"`
					OS       string `xml:"os-version,attr"`
					UUID     string `xml:"uuid,attr"`
				} `xml:"device"`
				Process struct {
					Name string `xml:"name,attr"`
					PID  string `xml:"pid,attr"`
				} `xml:"process"`
			} `xml:"target"`
			Summary struct {
				Start     string `xml:"start-date"`
				End       string `xml:"end-date"`
				Duration  string `xml:"duration"`
				EndReason string `xml:"end-reason"`
				TimeLimit string `xml:"time-limit"`
				Template  string `xml:"template-name"`
			} `xml:"summary"`
		} `xml:"info"`
		Data struct {
			Tables []struct {
				Attrs []xml.Attr `xml:",any,attr"`
			} `xml:"table"`
		} `xml:"data"`
	} `xml:"run"`
}

// ParseTOC reads `xctrace export --toc` output.
func ParseTOC(r io.Reader) (TOC, error) {
	var raw tocXML
	if err := xml.NewDecoder(r).Decode(&raw); err != nil {
		return TOC{}, fmt.Errorf("TOC XML: %w", err)
	}
	var toc TOC
	for _, rr := range raw.Runs {
		run := Run{
			Device: Device{
				Platform: rr.Info.Target.Device.Platform, Model: rr.Info.Target.Device.Model,
				OS: rr.Info.Target.Device.OS, UDID: rr.Info.Target.Device.UUID,
			},
			Process:   Process{Name: rr.Info.Target.Process.Name},
			EndReason: strings.TrimSpace(rr.Info.Summary.EndReason),
			TimeLimit: strings.TrimSpace(rr.Info.Summary.TimeLimit),
			Template:  strings.TrimSpace(rr.Info.Summary.Template),
		}
		run.Number, _ = strconv.Atoi(rr.Number)
		run.Process.PID, _ = strconv.Atoi(rr.Info.Target.Process.PID)
		if s := strings.TrimSpace(rr.Info.Summary.Start); s != "" {
			t, err := time.Parse(time.RFC3339Nano, s)
			if err != nil {
				return TOC{}, fmt.Errorf("TOC start-date %q: %w", s, err)
			}
			run.Start = t
		}
		if s := strings.TrimSpace(rr.Info.Summary.End); s != "" {
			if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
				run.End = t
			}
		}
		if s := strings.TrimSpace(rr.Info.Summary.Duration); s != "" {
			d, err := strconv.ParseFloat(s, 64)
			if err != nil {
				return TOC{}, fmt.Errorf("TOC duration %q: %w", s, err)
			}
			run.DurationS = d
		}
		for _, t := range rr.Data.Tables {
			tb := Table{Attrs: map[string]string{}}
			for _, a := range t.Attrs {
				if a.Name.Local == "schema" {
					tb.Schema = a.Value
					continue
				}
				tb.Attrs[a.Name.Local] = a.Value
			}
			if tb.Schema != "" {
				run.Tables = append(run.Tables, tb)
			}
		}
		toc.Runs = append(toc.Runs, run)
	}
	return toc, nil
}

// Run returns the run numbered n (1 is the first recording).
func (t TOC) Run(n int) (Run, bool) {
	for _, r := range t.Runs {
		if r.Number == n {
			return r, true
		}
	}
	return Run{}, false
}

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/henderson-tech/vybava/internal/journeys"
	"github.com/henderson-tech/vybava/internal/runx"
	"github.com/spf13/cobra"
)

func (rt *runtime) journeysApplet() *cobra.Command {
	c := rt.journeysCommand("journeys")
	c.SilenceUsage = true
	c.SilenceErrors = true
	c.SetOut(rt.stdout)
	c.SetErr(rt.stderr)
	c.PersistentFlags().BoolVar(&rt.json, "json", false, "emit versioned JSON envelope")
	return c
}
func (rt *runtime) journeysCommand(use string) *cobra.Command {
	var root, dir, private, adapter string
	c := &cobra.Command{Use: use, Short: "Validate human journeys, freeze plans and preserve immutable attempts"}
	c.PersistentFlags().StringVar(&root, "root", ".", "repository root")
	c.PersistentFlags().StringVar(&dir, "library", ".user-journeys-scripts", "library relative to root")
	c.PersistentFlags().StringVar(&private, "private", "", "private evidence root (required for records)")
	c.PersistentFlags().StringVar(&adapter, "adapter", "fixit", "registered adapter ID")
	finish := func(verb string, data interface{}, err error) error {
		diagnostics := []journeys.Diagnostic{}
		code := 0
		if err != nil {
			code = 2
			var p *journeys.Problem
			if errors.As(err, &p) {
				diagnostics = append(diagnostics, p.Diagnostic)
				switch p.Code {
				case "CAPABILITY_MISSING", "PLAN_STALE", "DEVICE_AMBIGUOUS", "NATIVE_BUILD_STALE", "TARGET_UNSAFE", "OUTCOME_UNSTABLE":
					code = 3
				case "ADAPTER_PROTOCOL", "JOURNAL_CORRUPT", "JOURNAL_TRUNCATED", "WRITER_BUSY":
					code = 4
				}
			} else {
				diagnostics = append(diagnostics, journeys.Diagnostic{Code: "IO_ERROR", Message: "operation failed: " + err.Error()})
				code = 4
			}
		}
		if rt.json {
			if e := writeJSON(rt.stdout, struct {
				V           int                   `json:"v"`
				OK          bool                  `json:"ok"`
				Verb        string                `json:"verb"`
				Data        interface{}           `json:"data"`
				Diagnostics []journeys.Diagnostic `json:"diagnostics"`
				Next        []string              `json:"next"`
			}{1, err == nil, verb, data, diagnostics, []string{}}); e != nil {
				return e
			}
		} else {
			if data != nil {
				b, e := json.MarshalIndent(data, "", "  ")
				if e != nil {
					return e
				}
				fmt.Fprintln(rt.stdout, string(b))
			}
			for _, d := range diagnostics {
				fmt.Fprintf(rt.stderr, "%s: %s\n", d.Code, d.Message)
			}
		}
		if code != 0 {
			return runx.ExitError{Code: code}
		}
		return nil
	}
	load := func() (*journeys.Library, error) {
		l, err := journeys.Load(root, dir)
		if err != nil {
			return l, err
		}
		return l, l.Valid()
	}
	store := func() (journeys.Store, error) {
		if private == "" {
			return journeys.Store{}, fmt.Errorf("--private must name the private exports directory")
		}
		p, err := journeys.PrivatePath(root, private)
		if err != nil {
			return journeys.Store{}, err
		}
		return journeys.Store{Root: p}, nil
	}
	for _, verb := range []string{"lint", "list"} {
		c.AddCommand(&cobra.Command{Use: verb, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
			l, err := load()
			if l == nil {
				return finish(cmd.Name(), nil, err)
			}
			if cmd.Name() == "lint" {
				return finish(cmd.Name(), struct {
					Documents   int                   `json:"documents"`
					Scenarios   int                   `json:"scenarios"`
					Candidates  int                   `json:"candidateModeCells"`
					Diagnostics []journeys.Diagnostic `json:"diagnostics"`
				}{len(l.Documents), len(l.Scenarios), l.CandidateCount(), l.Diagnostics}, err)
			}
			return finish(cmd.Name(), l.Documents, err)
		}})
	}
	c.AddCommand(&cobra.Command{Use: "show <name>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		l, err := load()
		if err != nil {
			return finish("show", nil, err)
		}
		paths, err := l.Compose(args[0])
		return finish("show", paths, err)
	}})
	for _, verb := range []string{"fmt", "index"} {
		var check bool
		sub := &cobra.Command{Use: verb, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
			l, err := load()
			if err != nil {
				return finish(cmd.Name(), nil, err)
			}
			changed := []string{}
			type file struct {
				path string
				b    []byte
			}
			files := []file{}
			if cmd.Name() == "index" {
				parent, err := journeys.SafePath(root, dir)
				if err != nil {
					return finish(cmd.Name(), nil, err)
				}
				path := filepath.Join(parent, "INDEX.md")
				info, err := os.Lstat(path)
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					return finish(cmd.Name(), nil, err)
				}
				if info != nil && info.Mode()&os.ModeSymlink != 0 {
					return finish(cmd.Name(), nil, &journeys.Problem{Diagnostic: journeys.Diagnostic{Code: "TARGET_UNSAFE", Message: "index destination is a symlink"}})
				}
				files = append(files, file{path, l.Index()})
			} else {
				for _, d := range l.Documents {
					b, err := journeys.Format(d)
					if err != nil {
						return finish(cmd.Name(), nil, err)
					}
					files = append(files, file{filepath.Join(root, d.Path), b})
				}
			}
			for _, f := range files {
				old, err := os.ReadFile(f.path)
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					return finish(cmd.Name(), nil, err)
				}
				if bytes.Equal(old, f.b) {
					continue
				}
				changed = append(changed, f.path)
				if !check {
					if err := os.WriteFile(f.path, f.b, 0644); err != nil {
						return finish(cmd.Name(), nil, err)
					}
				}
			}
			if check && len(changed) > 0 {
				err = &journeys.Problem{Diagnostic: journeys.Diagnostic{Code: "DOCUMENT_INVALID", Message: "generated formatting/index drift"}}
			}
			return finish(cmd.Name(), changed, err)
		}}
		sub.Flags().BoolVar(&check, "check", false, "read-only drift check")
		c.AddCommand(sub)
	}
	var coveragePlan string
	coverage := &cobra.Command{Use: "coverage", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		l, err := load()
		if err != nil {
			return finish("coverage", nil, err)
		}
		var p *journeys.Plan
		s := journeys.Store{}
		if coveragePlan != "" {
			p = &journeys.Plan{}
			if err := journeys.ReadJSON(coveragePlan, p); err != nil {
				return finish("coverage", nil, err)
			}
			s, err = store()
			if err != nil {
				return finish("coverage", nil, err)
			}
		}
		var current *journeys.Snapshot
		if p != nil && p.Adapter != "" {
			// Matching startup pins alone do not establish live evidence freshness.
			receipt, probeErr := journeys.Invoke(root, p.Adapter, journeys.Request{Version: 1, Operation: "readiness", Plan: p})
			if probeErr == nil {
				current = receipt.Snapshot
			}
		}
		r, err := s.CoverageAt(l, p, current)
		return finish("coverage", r, err)
	}}
	coverage.Flags().StringVar(&coveragePlan, "plan", "", "frozen plan JSON")
	c.AddCommand(coverage)
	for _, verb := range []string{"doctor", "devices"} {
		var boundPlan string
		sub := &cobra.Command{Use: verb, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
			var p *journeys.Plan
			if boundPlan != "" {
				p = &journeys.Plan{}
				if err := journeys.ReadJSON(boundPlan, p); err != nil {
					return finish(cmd.Name(), nil, err)
				}
				l, err := load()
				if err != nil {
					return finish(cmd.Name(), nil, err)
				}
				if err := p.Validate(l); err != nil {
					return finish(cmd.Name(), nil, err)
				}
				if err := p.CheckSeal(); err != nil {
					return finish(cmd.Name(), nil, err)
				}
				if p.Adapter != adapter {
					return finish(cmd.Name(), nil, fmt.Errorf("saved plan uses a different adapter"))
				}
			}
			r, err := journeys.Invoke(root, adapter, journeys.Request{Version: 1, Operation: cmd.Name(), Plan: p})
			return finish(cmd.Name(), r, err)
		}}
		sub.Flags().StringVar(&boundPlan, "plan", "", "read-only checks against explicit saved actor/device bindings")
		c.AddCommand(sub)
	}
	var planInput, planOut string
	plan := &cobra.Command{Use: "plan", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		l, err := load()
		if err != nil {
			return finish("plan", nil, err)
		}
		var p journeys.Plan
		if err := journeys.ReadJSON(planInput, &p); err != nil {
			return finish("plan", nil, err)
		}
		p.Adapter = adapter
		if err := p.Seal(l); err != nil {
			return finish("plan", nil, err)
		}
		if planOut == "" {
			return finish("plan", nil, fmt.Errorf("--out is required"))
		}
		output, err := journeys.PrivatePath(root, planOut)
		if err != nil {
			return finish("plan", nil, err)
		}
		b, err := json.MarshalIndent(p, "", "  ")
		if err != nil {
			return finish("plan", nil, err)
		}
		f, err := os.OpenFile(output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return finish("plan", nil, err)
		}
		_, err = f.Write(append(b, '\n'))
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
		return finish("plan", map[string]string{"id": p.ID, "hash": p.Hash, "path": planOut}, err)
	}}
	plan.Flags().StringVar(&planInput, "input", "", "typed draft plan JSON")
	plan.Flags().StringVar(&planOut, "out", "", "new private plan path")
	c.AddCommand(plan)
	var probeOut string
	probe := &cobra.Command{Use: "probe <plan.json> <request.json>", Short: "Read an independent outcome through the registered adapter", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		l, err := load()
		if err != nil {
			return finish("probe", nil, err)
		}
		var p journeys.Plan
		if err := journeys.ReadJSON(args[0], &p); err != nil {
			return finish("probe", nil, err)
		}
		var input json.RawMessage
		if err := journeys.ReadJSON(args[1], &input); err != nil {
			return finish("probe", nil, err)
		}
		if probeOut == "" {
			return finish("probe", nil, fmt.Errorf("--out must name a new private receipt"))
		}
		record, err := journeys.Probe(root, l, p, input, probeOut)
		return finish("probe", record, err)
	}}
	probe.Flags().StringVar(&probeOut, "out", "", "new private receipt path; failures are preserved too")
	c.AddCommand(probe)
	var apply bool
	start := &cobra.Command{Use: "start <plan.json>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		l, err := load()
		if err != nil {
			return finish("start", nil, err)
		}
		s, err := store()
		if err != nil {
			return finish("start", nil, err)
		}
		var p journeys.Plan
		if err := journeys.ReadJSON(args[0], &p); err != nil {
			return finish("start", nil, err)
		}
		result, err := s.Start(root, l, p, apply)
		return finish("start", result, err)
	}}
	start.Flags().BoolVar(&apply, "apply", false, "execute the unchanged saved plan")
	c.AddCommand(start)
	var phase, retest string
	begin := &cobra.Command{Use: "case <plan.json>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		s, err := store()
		if err != nil {
			return finish("case", nil, err)
		}
		l, err := load()
		if err != nil {
			return finish("case", nil, err)
		}
		var p journeys.Plan
		if err := journeys.ReadJSON(args[0], &p); err != nil {
			return finish("case", nil, err)
		}
		if err := p.Validate(l); err != nil {
			return finish("case", nil, err)
		}
		a, err := s.Begin(p, phase, retest)
		return finish("case", map[string]string{"attemptId": a.ID, "runId": a.RunID}, err)
	}}
	begin.Flags().StringVar(&phase, "phase", "preflight", "preflight or journey")
	begin.Flags().StringVar(&retest, "retest-of", "", "finished earlier attempt ID")
	c.AddCommand(begin)
	for _, verb := range []string{"observe", "verify", "verdict"} {
		sub := &cobra.Command{Use: verb + " <attempt-id> <event.json>", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
			s, err := store()
			if err != nil {
				return finish(cmd.Name(), nil, err)
			}
			var e journeys.Event
			if err := journeys.ReadJSON(args[1], &e); err != nil {
				return finish(cmd.Name(), nil, err)
			}
			if cmd.Name() == "verify" {
				e.Kind = "verification"
			}
			if cmd.Name() == "verdict" {
				e.Kind = "verdict"
			}
			e, err = s.Append(args[0], e)
			return finish(cmd.Name(), map[string]int{"sequence": e.Sequence}, err)
		}}
		c.AddCommand(sub)
	}
	var recoverTail bool
	resume := &cobra.Command{Use: "resume <attempt-id>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		s, err := store()
		if err != nil {
			return finish("resume", nil, err)
		}
		a, events, recovered, err := s.Read(args[0], recoverTail)
		return finish("resume", struct {
			Attempt   string `json:"attemptId"`
			Events    int    `json:"events"`
			Recovered int    `json:"recoveredTailBytes"`
		}{a.ID, len(events), recovered}, err)
	}}
	resume.Flags().BoolVar(&recoverTail, "recover", false, "archive and remove an incomplete final journal record")
	c.AddCommand(resume)
	var export string
	end := &cobra.Command{Use: "finish <attempt-id>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		s, err := store()
		if err != nil {
			return finish("finish", nil, err)
		}
		summary, err := s.Finish(args[0])
		if err == nil && export != "" {
			err = s.Export(args[0], export)
		}
		return finish("finish", summary, err)
	}}
	end.Flags().StringVar(&export, "export", "", "new sanitized summary file")
	c.AddCommand(end)
	var publicationMode, presentationPath, publicationExport string
	var publishApply bool
	var captureApprovals []string
	pub := &cobra.Command{Use: "publish <attempt-id>", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		l, err := load()
		if err != nil {
			return finish("publish", nil, err)
		}
		s, err := store()
		if err != nil {
			return finish("publish", nil, err)
		}
		var presentation *journeys.Presentation
		if presentationPath != "" {
			presentation = &journeys.Presentation{}
			if err := journeys.ReadJSON(presentationPath, presentation); err != nil {
				return finish("publish", nil, err)
			}
		}
		if !publishApply {
			if publicationExport != "" {
				return finish("publish", nil, fmt.Errorf("--export requires --apply and a completed publication"))
			}
			p, err := s.PreparePublication(l, args[0], publicationMode, presentation)
			return finish("publish", p, err)
		}
		result, err := s.Publish(root, l, args[0], publicationMode, presentation, true, captureApprovals)
		if err == nil && publicationExport != "" {
			err = s.ExportPublication(l, args[0], publicationMode, presentation, publicationExport)
		}
		return finish("publish", result, err)
	}}
	pub.Flags().StringVar(&publicationMode, "mode", "", "one selected mode; its sealed story owns the publication")
	pub.Flags().StringVar(&presentationPath, "presentation", "", "reviewed public notes and image hashes, tied to the finished journal")
	pub.Flags().StringVar(&publicationExport, "export", "", "new sanitized publication references file beside the finished summary")
	pub.Flags().BoolVar(&publishApply, "apply", false, "publish the reviewed projection through the registered adapter")
	pub.Flags().StringArrayVar(&captureApprovals, "approve-capture", nil, "SHA256 of an inspected converted capture (repeatable)")
	c.AddCommand(pub)
	return c
}

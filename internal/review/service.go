package review

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Service struct {
	Store  *Store
	Runner Runner
	// WorkRoot holds per-run private workspaces; they are removed after the run unless KeepWorkspace is set.
	WorkRoot      string
	KeepWorkspace bool
	// Parallel bounds concurrent model calls within one review (default 4).
	Parallel int
}

const (
	PassDefect    = "defect"
	PassTriggers  = "triggers"
	PassStructure = "structure"
	checkEffort   = "medium"
)

// Options configure one review. Effort is "auto" (route by diff, escalate low→high on a high-severity finding),
// "low" or "high" and applies to the defect pass. Passes defaults to all three passes.
type Options struct {
	Effort    string    `json:"effort"`
	Purpose   string    `json:"purpose,omitempty"`
	Passes    []string  `json:"passes,omitempty"`
	Context   []string  `json:"context,omitempty"`
	Repos     []RepoRef `json:"repos,omitempty"`
	MergeWith []string  `json:"merge_with,omitempty"`
	// PriorBefore limits carried-forward findings to model/check findings of review runs created before it
	// (an eval cutoff), so a re-review of a past commit cannot see what was found or added later.
	PriorBefore string `json:"prior_before,omitempty"`
}

func (o Options) has(pass string) bool {
	if len(o.Passes) == 0 {
		return true
	}
	for _, p := range o.Passes {
		if p == pass {
			return true
		}
	}
	return false
}

type Report struct {
	Run       Run    `json:"run"`
	Earlier   []Run  `json:"earlier_runs,omitempty"`
	Workspace string `json:"workspace,omitempty"`
}

func (s *Service) Run(ctx context.Context, t Target, o Options) (Report, error) {
	started := time.Now()
	root, err := filepath.Abs(s.WorkRoot)
	if err != nil {
		return Report{}, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return Report{}, err
	}
	work, err := os.MkdirTemp(root, "run-")
	if err != nil {
		return Report{}, err
	}
	if !s.KeepWorkspace {
		defer os.RemoveAll(work)
	}
	codeDir := filepath.Join(work, "repo")
	if err := os.Mkdir(codeDir, 0o700); err != nil {
		return Report{}, err
	}
	m, err := t.Prepare(ctx, codeDir)
	if err != nil {
		return Report{}, err
	}
	if err := writeWorkspace(work, m); err != nil {
		return Report{}, err
	}
	repos, err := exportRepos(ctx, work, o.Repos)
	if err != nil {
		return Report{}, err
	}
	contextFiles, err := copyContext(work, o.Context)
	if err != nil {
		return Report{}, err
	}
	target := t.Describe()
	if len(repos) > 0 {
		target["with_repos"] = strings.Join(repos, ",")
	}
	fds := parseDiff(m.Diff)
	facts := s.collectFacts(ctx, work, m, fds, o)
	var factsText bytes.Buffer
	writeFacts(&factsText, facts, repos, contextFiles, consumerSearch(work, fds), structureFacts(work, fds))
	if err := os.WriteFile(filepath.Join(work, "facts.md"), factsText.Bytes(), 0o600); err != nil {
		return Report{}, err
	}
	var files []string
	for _, fd := range fds {
		files = append(files, fd.Path)
	}
	prior, err := s.Store.PriorFindings(ctx, target["remote"], files, 20, o.PriorBefore)
	if err != nil {
		return Report{}, err
	}
	if len(prior) > 0 {
		var b bytes.Buffer
		writePriorFindings(&b, prior)
		if err := os.WriteFile(filepath.Join(work, "prior_findings.md"), b.Bytes(), 0o600); err != nil {
			return Report{}, err
		}
	}

	route := RouteDiff(m.Diff)
	effort := o.Effort
	if effort == "auto" || effort == "" {
		effort = route.Effort
	}
	purpose := o.Purpose
	if purpose == "" {
		purpose = "review"
	}
	base := Run{TargetKind: t.Kind(), Target: target, Issue: m.Issue, Diff: string(m.Diff), Route: route, Model: s.Runner.Model(),
		Purpose: purpose, Reviewed: reviewedFiles(codeDir, fds)}

	parallel := s.Parallel
	if parallel < 1 {
		parallel = 4
	}
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	var defect defectResult
	var checks []passResult
	var structure *passResult
	var mu sync.Mutex
	if o.has(PassDefect) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defect = s.defectPass(ctx, sem, work, effort, o.Effort == "auto" || o.Effort == "", base)
		}()
	}
	if o.has(PassTriggers) {
		for _, tr := range facts.Triggers {
			wg.Add(1)
			go func(tr Trigger) {
				defer wg.Done()
				r := s.checkPass(ctx, sem, work, tr)
				mu.Lock()
				checks = append(checks, r)
				mu.Unlock()
			}(tr)
		}
	}
	if o.has(PassStructure) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := s.structurePass(ctx, sem, work)
			structure = &r
		}()
	}
	wg.Wait()
	if defect.err != nil {
		return Report{}, defect.err
	}

	final := base
	final.Effort = effort
	final.Status, final.Complete = "succeeded", true
	final.Findings = append([]StoredFinding{}, facts.Findings...)
	final.Checks = append([]Check{}, facts.Checks...)
	var earlier []Run
	add := func(c CallResult) {
		final.ToolUses += c.ToolUses
		final.Warnings = append(final.Warnings, c.Warnings...)
		if final.ObservedModel == "" {
			final.ObservedModel = c.ObservedModel
		}
	}
	if o.has(PassDefect) {
		if defect.earlier != nil {
			earlier = append(earlier, *defect.earlier)
			final.EscalatedFrom = defect.earlier.ID
		}
		final.Effort = defect.effort
		add(defect.call)
		final.Inspected, final.NotChecked = defect.out.InspectedFiles, defect.out.NotChecked
		if defect.callErr != nil {
			final.Status, final.Error = "failed", defect.callErr.Error()
		}
		for _, f := range defect.out.Findings {
			final.Findings = append(final.Findings, StoredFinding{Pass: PassDefect, Category: "defect", Finding: f})
		}
	}
	sortChecks(checks)
	for _, c := range checks {
		add(c.call)
		final.Checks = append(final.Checks, c.check)
		if c.check.Conclusion == "not_run" || (c.check.Conclusion != "cannot_determine" && len(c.check.Evidence) == 0) {
			final.Complete = false
		}
		for _, f := range c.findings {
			final.Findings = append(final.Findings, StoredFinding{Pass: "trigger:" + c.check.Name, Category: "defect", Finding: f})
		}
	}
	if structure != nil {
		add(structure.call)
		final.Checks = append(final.Checks, structure.check)
		if structure.check.Conclusion == "not_run" {
			final.Complete = false
		}
		for _, f := range structure.findings {
			final.Findings = append(final.Findings, StoredFinding{Pass: PassStructure, Category: "structure", Finding: f})
		}
	}
	if final.Inspected == nil {
		final.Inspected = []string{}
	}
	final.Seconds = time.Since(started).Seconds()
	if err := s.Store.SaveRun(ctx, &final); err != nil {
		return Report{}, err
	}
	rep := Report{Run: final, Earlier: earlier}
	if s.KeepWorkspace {
		rep.Workspace = work
	}
	return rep, nil
}

func (s *Service) collectFacts(ctx context.Context, work string, m Material, fds []fileDiff, o Options) Facts {
	f := Facts{Triggers: detectTriggers(fds)}
	if t, ok := newTestsTrigger(m.CodeDir, fds); ok {
		f.Triggers = append(f.Triggers, t)
	}
	for _, run := range []func() (Check, []StoredFinding){
		func() (Check, []StoredFinding) { return suppressionCheck(fds) },
		func() (Check, []StoredFinding) { return removedDefinitionCheck(work, fds) },
		func() (Check, []StoredFinding) { return mergeCheck(ctx, m.Repo, m.Head, o.MergeWith) },
	} {
		c, fs := run()
		f.Checks = append(f.Checks, c)
		f.Findings = append(f.Findings, fs...)
	}
	return f
}

type defectResult struct {
	out     Output
	call    CallResult
	effort  string
	earlier *Run
	callErr error // a failed model call: the run is recorded as failed
	err     error // a ledger error: the review cannot be recorded
}

// defectPass is the broad review. A low result with a high-severity finding is saved as an earlier run and
// re-reviewed at high effort when the effort was routed automatically.
func (s *Service) defectPass(ctx context.Context, sem chan struct{}, work, effort string, auto bool, base Run) defectResult {
	call := func(e string) (CallResult, Output, error) {
		sem <- struct{}{}
		defer func() { <-sem }()
		c, err := s.Runner.Call(ctx, Call{Pass: PassDefect, WorkDir: work, Effort: e, Prompt: defectPrompt, Schema: findingsSchema})
		if err != nil {
			return c, Output{Findings: []Finding{}, InspectedFiles: []string{}}, err
		}
		o, err := decodeOutput(c.Output)
		return c, o, err
	}
	c, out, err := call(effort)
	res := defectResult{out: out, call: c, effort: effort, callErr: err}
	if err != nil || !auto || effort != "low" || !shouldEscalate(out.Findings) {
		return res
	}
	low := base
	low.Effort, low.Status, low.Complete = "low", "succeeded", true
	low.ObservedModel, low.ToolUses, low.Seconds, low.Warnings = c.ObservedModel, c.ToolUses, c.Duration.Seconds(), c.Warnings
	low.Inspected, low.NotChecked = out.InspectedFiles, out.NotChecked
	for _, f := range out.Findings {
		low.Findings = append(low.Findings, StoredFinding{Pass: PassDefect, Category: "defect", Finding: f})
	}
	if err := s.Store.SaveRun(ctx, &low); err != nil {
		return defectResult{err: err}
	}
	c, out, err = call("high")
	return defectResult{out: out, call: c, effort: "high", earlier: &low, callErr: err}
}

type passResult struct {
	check    Check
	findings []Finding
	call     CallResult
}

type checkOutput struct {
	Targets    []string  `json:"targets_checked"`
	Conclusion string    `json:"conclusion"`
	Evidence   []string  `json:"evidence"`
	NotChecked string    `json:"not_checked"`
	Findings   []Finding `json:"findings"`
}

func (s *Service) checkPass(ctx context.Context, sem chan struct{}, work string, t Trigger) passResult {
	sem <- struct{}{}
	defer func() { <-sem }()
	r := passResult{check: Check{Name: t.Name, Kind: "trigger", Locations: t.Locations}}
	c, err := s.Runner.Call(ctx, Call{Pass: "trigger:" + t.Name, WorkDir: work, Effort: checkEffort, Prompt: checkPrompt(t), Schema: checkSchema})
	r.call = c
	var out checkOutput
	if err == nil {
		err = json.Unmarshal(c.Output, &out)
	}
	if err != nil || !oneOf(out.Conclusion, "issue", "no_issue", "cannot_determine") {
		r.check.Conclusion = "not_run"
		if err != nil {
			r.check.Error = err.Error()
		} else {
			r.check.Error = fmt.Sprintf("invalid conclusion %q", out.Conclusion)
		}
		return r
	}
	r.check.Conclusion, r.check.Targets, r.check.Evidence, r.check.NotChecked = out.Conclusion, out.Targets, out.Evidence, out.NotChecked
	r.findings = out.Findings
	return r
}

func (s *Service) structurePass(ctx context.Context, sem chan struct{}, work string) passResult {
	sem <- struct{}{}
	defer func() { <-sem }()
	r := passResult{check: Check{Name: PassStructure, Kind: "pass", Conclusion: "no_issue"}}
	c, err := s.Runner.Call(ctx, Call{Pass: PassStructure, WorkDir: work, Effort: checkEffort, Prompt: structurePrompt, Schema: findingsSchema})
	r.call = c
	var out Output
	if err == nil {
		out, err = decodeOutput(c.Output)
	}
	if err != nil {
		r.check.Conclusion, r.check.Error = "not_run", err.Error()
		return r
	}
	r.check.Targets, r.check.NotChecked = out.InspectedFiles, out.NotChecked
	if len(out.Findings) > 0 {
		r.check.Conclusion = "issue"
	}
	r.findings = out.Findings
	return r
}

func sortChecks(cs []passResult) {
	for i := 1; i < len(cs); i++ {
		for j := i; j > 0 && cs[j].check.Name < cs[j-1].check.Name; j-- {
			cs[j], cs[j-1] = cs[j-1], cs[j]
		}
	}
}

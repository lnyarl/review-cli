package review

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Suite is a fixed set of past changes with expected findings (candidates from past review threads,
// not ground truth). Paths in a suite file are relative to the suite file.
type Suite struct {
	Name  string      `json:"name"`
	Repo  string      `json:"repo"`
	Cases []SuiteCase `json:"cases"`
}

// SuiteCase overrides the suite repo with Repo, and adds sibling repos and context files (relative to the suite).
type SuiteCase struct {
	ID        string     `json:"id"`
	Kind      string     `json:"kind"` // defect | normal
	Repo      string     `json:"repo,omitempty"`
	Base      string     `json:"base"`
	Head      string     `json:"head"`
	Cutoff    string     `json:"cutoff,omitempty"` // RFC3339; earlier findings after this are not carried forward
	IssueFile string     `json:"issue_file"`
	WithRepos []RepoRef  `json:"with_repos,omitempty"`
	Context   []string   `json:"context,omitempty"`
	Expected  []Expected `json:"expected"`
}

// Expected is a past review comment. Detail is its full text, which the judge compares with findings.
type Expected struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Summary string `json:"summary"`
	Detail  string `json:"detail,omitempty"`
}

// JudgeMatch says whether a finding has the same cause as an expected comment (1-based index).
type JudgeMatch struct {
	Expected   int      `json:"expected"`
	Verdict    string   `json:"verdict"` // same_cause | partial | none
	FindingIDs []string `json:"finding_ids"`
	Reason     string   `json:"reason"`
}

// nearLines is how close a finding must be to an expected location to count as a candidate hit.
// It is a hint for judging, not a correctness verdict.
const nearLines = 30

type CaseResult struct {
	ID          string       `json:"id"`
	Kind        string       `json:"kind"`
	RunID       string       `json:"run_id"`
	Effort      string       `json:"effort"`
	Escalated   bool         `json:"escalated"`
	Status      string       `json:"status"`
	Seconds     float64      `json:"seconds"`
	Findings    int          `json:"findings"`
	Expected    int          `json:"expected"`
	NearHits    int          `json:"near_hits"`
	NearHitList []int        `json:"near_hit_expected"`
	Matched     int          `json:"matched"`
	Partial     int          `json:"partial"`
	Matches     []JudgeMatch `json:"matches,omitempty"`
	Complete    bool         `json:"complete"`
	Error       string       `json:"error,omitempty"`
	JudgeError  string       `json:"judge_error,omitempty"`
}

type EvalResult struct {
	Suite          string       `json:"suite"`
	Model          string       `json:"model"`
	Options        Options      `json:"options"`
	Cases          []CaseResult `json:"cases"`
	NearHits       int          `json:"near_hits"`
	Matched        int          `json:"matched"`
	Partial        int          `json:"partial"`
	Expected       int          `json:"expected"`
	Incomplete     int          `json:"incomplete"`
	NormalFindings int          `json:"normal_findings"`
	Seconds        float64      `json:"seconds"`
	Failed         int          `json:"failed"`
}

func LoadSuite(path string) (Suite, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Suite{}, "", err
	}
	var s Suite
	if err := json.Unmarshal(data, &s); err != nil {
		return Suite{}, "", fmt.Errorf("invalid suite: %w", err)
	}
	if s.Name == "" || len(s.Cases) == 0 {
		return Suite{}, "", fmt.Errorf("invalid suite: name and cases are required")
	}
	for _, c := range s.Cases {
		if c.Repo == "" && s.Repo == "" {
			return Suite{}, "", fmt.Errorf("invalid suite: case %s has no repo", c.ID)
		}
	}
	return s, filepath.Dir(path), nil
}

// Eval reviews every case once, up to parallel at a time, judges whether findings match the expected comments,
// and returns the previous eval of the suite for comparison.
func (s *Service) Eval(ctx context.Context, suite Suite, dir string, o Options, parallel int) (EvalResult, json.RawMessage, error) {
	previous, err := s.Store.LastEval(ctx, suite.Name)
	if err != nil {
		return EvalResult{}, nil, err
	}
	issues := make([]string, len(suite.Cases))
	for i, c := range suite.Cases {
		if c.IssueFile != "" {
			data, err := os.ReadFile(filepath.Join(dir, c.IssueFile))
			if err != nil {
				return EvalResult{}, previous, err
			}
			issues[i] = string(data)
		}
	}
	if parallel < 1 {
		parallel = 1
	}
	cases := make([]CaseResult, len(suite.Cases))
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, c := range suite.Cases {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, c SuiteCase) {
			defer func() { <-sem; wg.Done() }()
			cases[i] = s.evalCase(ctx, suite.Repo, dir, c, issues[i], o)
		}(i, c)
	}
	wg.Wait()
	res := EvalResult{Suite: suite.Name, Model: s.Runner.Model(), Options: o, Cases: cases}
	for i, cr := range cases {
		if cr.Status != "succeeded" {
			res.Failed++
		}
		if suite.Cases[i].Kind == "normal" {
			res.NormalFindings += cr.Findings
		}
		if !cr.Complete {
			res.Incomplete++
		}
		res.NearHits += cr.NearHits
		res.Matched += cr.Matched
		res.Partial += cr.Partial
		res.Expected += cr.Expected
		res.Seconds += cr.Seconds
	}
	if _, err := s.Store.SaveEval(ctx, suite.Name, o, res); err != nil {
		return res, previous, err
	}
	return res, previous, nil
}

func (s *Service) evalCase(ctx context.Context, repo, dir string, c SuiteCase, issue string, o Options) CaseResult {
	cr := CaseResult{ID: c.ID, Kind: c.Kind, Expected: len(c.Expected), NearHitList: []int{}}
	if c.Repo != "" {
		repo = c.Repo
	}
	co := o
	co.Purpose = "eval" // re-reviews of past commits must not count as coverage
	co.PriorBefore = c.Cutoff
	if co.PriorBefore == "" {
		co.PriorBefore = "0000" // no cutoff: carry nothing forward rather than risk later knowledge
	}
	co.Repos = append(append([]RepoRef{}, o.Repos...), c.WithRepos...)
	co.Context = append([]string{}, o.Context...)
	for _, p := range c.Context {
		co.Context = append(co.Context, filepath.Join(dir, p))
	}
	rep, err := s.Run(ctx, &CommitRange{Repo: repo, Base: c.Base, Head: c.Head, Issue: issue}, co)
	if err != nil {
		cr.Status, cr.Error = "failed", err.Error()
		return cr
	}
	cr.RunID, cr.Effort, cr.Status, cr.Escalated = rep.Run.ID, rep.Run.Effort, rep.Run.Status, len(rep.Earlier) > 0
	cr.Error, cr.Findings, cr.Seconds, cr.Complete = rep.Run.Error, len(rep.Run.Findings), rep.Run.Seconds, rep.Run.Complete
	for i, e := range c.Expected {
		if anyNear(rep.Run.Findings, e) {
			cr.NearHits++
			cr.NearHitList = append(cr.NearHitList, i+1)
		}
	}
	if len(c.Expected) == 0 {
		return cr
	}
	matches, err := s.judge(ctx, c.Expected, rep.Run.Findings)
	if err != nil {
		cr.JudgeError = err.Error()
		return cr
	}
	cr.Matches = matches
	for _, m := range matches {
		switch m.Verdict {
		case "same_cause":
			cr.Matched++
		case "partial":
			cr.Partial++
		}
	}
	return cr
}

// judge asks the model whether each expected comment has a finding with the same cause, so a nearby finding
// on a different problem is not counted as found.
func (s *Service) judge(ctx context.Context, expected []Expected, findings []StoredFinding) ([]JudgeMatch, error) {
	root, err := filepath.Abs(s.WorkRoot)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	work, err := os.MkdirTemp(root, "judge-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	type exp struct {
		N int `json:"n"`
		Expected
	}
	in := struct {
		Expected []exp           `json:"expected"`
		Findings []StoredFinding `json:"findings"`
	}{Findings: findings}
	for i, e := range expected {
		in.Expected = append(in.Expected, exp{i + 1, e})
	}
	data, _ := json.MarshalIndent(in, "", " ")
	if err := os.WriteFile(filepath.Join(work, "judge.json"), data, 0o600); err != nil {
		return nil, err
	}
	c, err := s.Runner.Call(ctx, Call{Pass: "judge", WorkDir: work, Effort: checkEffort, Prompt: judgePrompt, Schema: judgeSchema})
	if err != nil {
		return nil, err
	}
	var out struct {
		Matches []JudgeMatch `json:"matches"`
	}
	if err := json.Unmarshal(c.Output, &out); err != nil {
		return nil, fmt.Errorf("invalid judge output: %w", err)
	}
	return out.Matches, nil
}

func anyNear(fs []StoredFinding, e Expected) bool {
	for _, f := range fs {
		if normPath(f.File) == normPath(e.File) && (e.Line == 0 || abs(f.Line-e.Line) <= nearLines) {
			return true
		}
	}
	return false
}

func normPath(p string) string {
	return strings.TrimPrefix(strings.TrimPrefix(filepath.ToSlash(p), "/"), "repo/")
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

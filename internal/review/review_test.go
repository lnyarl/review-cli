package review

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func gitT(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null"}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

// newRepo returns a repo with commits A (a.rb, old.rb, keep.rb) and B (a.rb changed).
func newRepo(t *testing.T) (repo, a, b string) {
	repo = t.TempDir()
	gitT(t, repo, "init", "-q", "-b", "main")
	gitT(t, repo, "remote", "add", "origin", "git@example:org/proj/repo")
	write(t, filepath.Join(repo, "app", "a.rb"), "v1\n")
	write(t, filepath.Join(repo, "old.rb"), "old\n")
	write(t, filepath.Join(repo, "keep.rb"), "keep\n")
	gitT(t, repo, "add", ".")
	gitT(t, repo, "commit", "-qm", "A")
	a = gitT(t, repo, "rev-parse", "HEAD")
	write(t, filepath.Join(repo, "app", "a.rb"), "v2\n")
	gitT(t, repo, "commit", "-qam", "B")
	b = gitT(t, repo, "rev-parse", "HEAD")
	return repo, a, b
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestCommitRangeExportsHeadWithoutGit(t *testing.T) {
	repo, a, b := newRepo(t)
	dst := t.TempDir()
	tg := &CommitRange{Repo: repo, Base: a, Head: "main", Issue: "issue"}
	m, err := tg.Prepare(context.Background(), dst)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, filepath.Join(dst, "app", "a.rb")); got != "v2\n" {
		t.Fatalf("head content = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dst, ".git")); !os.IsNotExist(err) {
		t.Fatal("export must not contain .git")
	}
	if !strings.Contains(string(m.Diff), "-v1") || !strings.Contains(string(m.Diff), "+v2") {
		t.Fatalf("diff = %s", m.Diff)
	}
	d := tg.Describe()
	if d["head"] != b || d["base"] != a || d["remote"] != "git@example:org/proj/repo" {
		t.Fatalf("describe = %v", d)
	}
}

func TestWorkingTreeOverlaysUncommittedChanges(t *testing.T) {
	repo, _, _ := newRepo(t)
	write(t, filepath.Join(repo, "app", "a.rb"), "v3\n")
	write(t, filepath.Join(repo, "new.rb"), "new\n")
	gitT(t, repo, "mv", "old.rb", "moved.rb")
	if err := os.Remove(filepath.Join(repo, "keep.rb")); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	tg := &WorkingTree{Repo: repo}
	m, err := tg.Prepare(context.Background(), dst)
	if err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(dst, "app", "a.rb")) != "v3\n" || read(t, filepath.Join(dst, "new.rb")) != "new\n" || read(t, filepath.Join(dst, "moved.rb")) != "old\n" {
		t.Fatal("working tree content not overlaid")
	}
	for _, gone := range []string{"keep.rb", "old.rb"} {
		if _, err := os.Stat(filepath.Join(dst, gone)); !os.IsNotExist(err) {
			t.Fatalf("%s should be removed from the export", gone)
		}
	}
	for _, want := range []string{"+v3", "+new", "keep.rb", "moved.rb"} {
		if !strings.Contains(string(m.Diff), want) {
			t.Fatalf("diff missing %q:\n%s", want, m.Diff)
		}
	}

	only := &WorkingTree{Repo: repo, Paths: []string{"new.rb"}}
	m, err = only.Prepare(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(m.Diff), "a.rb") || !strings.Contains(string(m.Diff), "+new") {
		t.Fatalf("paths filter not applied:\n%s", m.Diff)
	}
	if _, err := (&WorkingTree{Repo: repo, Paths: []string{"nochange.rb"}}).Prepare(context.Background(), t.TempDir()); err == nil {
		t.Fatal("unknown path must fail")
	}
}

func TestPatchAppliesStagedChangesOnBase(t *testing.T) {
	repo, _, b := newRepo(t)
	write(t, filepath.Join(repo, "app", "a.rb"), "staged\n")
	write(t, filepath.Join(repo, "added.rb"), "added\n")
	gitT(t, repo, "add", "app/a.rb", "added.rb")
	write(t, filepath.Join(repo, "app", "a.rb"), "unstaged\n") // must not leak into the review
	patch := gitT(t, repo, "diff", "--cached", "--binary") + "\n"
	dst := t.TempDir()
	tg := &Patch{Repo: repo, Base: "HEAD", Label: "staged", Diff: []byte(patch)}
	m, err := tg.Prepare(context.Background(), dst)
	if err != nil {
		t.Fatal(err)
	}
	if read(t, filepath.Join(dst, "app", "a.rb")) != "staged\n" || read(t, filepath.Join(dst, "added.rb")) != "added\n" {
		t.Fatal("export must be base plus the patch, without unstaged edits")
	}
	if _, err := os.Stat(filepath.Join(dst, ".git")); !os.IsNotExist(err) {
		t.Fatal("export must not contain .git")
	}
	d := tg.Describe()
	if string(m.Diff) != patch || d["base"] != b || d["label"] != "staged" || d["files"] != "added.rb,app/a.rb" || d["patch_sha256"] == "" {
		t.Fatalf("describe = %v", d)
	}
	if _, err := (&Patch{Repo: repo, Base: "HEAD", Diff: []byte("diff --git a/x b/x\n--- a/x\n+++ b/x\n@@ -1 +1 @@\n-no\n+yes\n")}).Prepare(context.Background(), t.TempDir()); err == nil {
		t.Fatal("a patch that does not apply must fail")
	}
	if _, err := (&Patch{Repo: repo, Base: "HEAD"}).Prepare(context.Background(), t.TempDir()); err == nil {
		t.Fatal("an empty patch must fail")
	}
}

func TestAdoPRRequiresMatchingRemote(t *testing.T) {
	repo, a, b := newRepo(t)
	dbPath := filepath.Join(t.TempDir(), "collector.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE raw_items(id INTEGER PRIMARY KEY, provider TEXT, external_id TEXT, event_json TEXT)`); err != nil {
		t.Fatal(err)
	}
	insert := func(id int, ssh string) {
		ev := map[string]any{"payload": map[string]any{"pull_request": map[string]any{"title": "T", "description": "D",
			"repository": map[string]any{"sshUrl": ssh}, "lastMergeSourceCommit": map[string]any{"commitId": b},
			"lastMergeTargetCommit": map[string]any{"commitId": a}}}}
		data, _ := json.Marshal(ev)
		if _, err := db.Exec(`INSERT INTO raw_items(provider,external_id,event_json) VALUES('azuredevops',?,?)`, fmt.Sprintf("x:pr:%d", id), string(data)); err != nil {
			t.Fatal(err)
		}
	}
	insert(7, "git@example:org/proj/repo")
	insert(8, "git@example:org/other/repo")
	db.Close()

	pr := &AdoPR{CollectorDB: dbPath, Repo: repo, PRID: 7}
	m, err := pr.Prepare(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.Issue, "PR 7: T") || !strings.Contains(string(m.Diff), "+v2") || pr.Describe()["head"] != b {
		t.Fatalf("issue=%q describe=%v", m.Issue, pr.Describe())
	}
	if _, err := (&AdoPR{CollectorDB: dbPath, Repo: repo, PRID: 8}).Prepare(context.Background(), t.TempDir()); err == nil || !strings.Contains(err.Error(), "unresolved_repository") {
		t.Fatalf("same repo name on another remote must be rejected, got %v", err)
	}
}

func TestOpenStoreWidensOldVerdicts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE verdicts(id INTEGER PRIMARY KEY AUTOINCREMENT, finding_id TEXT NOT NULL,
		verdict TEXT NOT NULL CHECK(verdict IN ('defect','false_positive','unsure','duplicate')), note TEXT NOT NULL, actor TEXT NOT NULL, created_at TEXT NOT NULL);
		INSERT INTO verdicts(finding_id,verdict,note,actor,created_at) VALUES('f-1','defect','','me','t')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := OpenStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM verdicts WHERE verdict='defect'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("old verdicts must survive: %d %v", n, err)
	}
	var ddl string
	if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE name='verdicts'`).Scan(&ddl); err != nil || !strings.Contains(ddl, "'useful'") {
		t.Fatalf("verdicts must accept useful after open: %s %v", ddl, err)
	}
}

func TestRouteDiff(t *testing.T) {
	small := "diff --git a/app/a.rb b/app/a.rb\n+x\n"
	if r := RouteDiff([]byte(small)); r.Effort != "low" {
		t.Fatalf("small diff routed %v", r)
	}
	if r := RouteDiff([]byte("diff --git a/db/migrate/1_x.rb b/db/migrate/1_x.rb\n+x\n")); r.Effort != "high" {
		t.Fatalf("migration routed %v", r)
	}
	if r := RouteDiff([]byte("diff --git a/spec/cache_spec.rb b/spec/cache_spec.rb\n+x\n")); r.Effort != "low" {
		t.Fatalf("spec-only risk path routed %v", r)
	}
	many := strings.Repeat("diff --git a/app/f.rb b/app/f.rb\n+x\n", routeMaxFiles)
	if r := RouteDiff([]byte(many)); r.Effort != "high" {
		t.Fatalf("many files routed %v", r)
	}
}

const streamOK = `{"type":"system","subtype":"init","model":"claude-sonnet-5-5","tools":["Read","Grep","Glob","StructuredOutput"],"mcp_servers":[]}
{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read"}]}}
{"type":"result","subtype":"success","is_error":false,"permission_denials":[],"structured_output":{"findings":[{"title":"t","severity":"high","confidence":"medium","file":"app/a.rb","line":3,"condition":"c","code_path":"p","impact":"i","evidence":"e"}],"inspected_files":["app/a.rb"],"not_checked":"none"}}
`

func TestParseClaudeStream(t *testing.T) {
	res, err := parseClaudeStream(streamOK)
	if err != nil {
		t.Fatal(err)
	}
	out, err := decodeOutput(res.Output)
	if err != nil || res.ObservedModel != "claude-sonnet-5-5" || res.ToolUses != 1 || len(out.Findings) != 1 || out.Findings[0].Line != 3 {
		t.Fatalf("parsed %+v %+v %v", res, out, err)
	}
	bad := map[string]string{
		"mcp":   strings.Replace(streamOK, `"mcp_servers":[]`, `"mcp_servers":[{"name":"x"}]`, 1),
		"tool":  strings.Replace(streamOK, `"Glob",`, `"Glob","Bash",`, 1),
		"error": strings.Replace(streamOK, `"is_error":false`, `"is_error":true`, 1),
		"empty": strings.SplitN(streamOK, "\n", 2)[0],
	}
	denied, err := parseClaudeStream(strings.Replace(streamOK, `"permission_denials":[]`, `"permission_denials":[{"tool_name":"Read"}]`, 1))
	if err != nil || len(denied.Warnings) != 1 || len(denied.Output) == 0 {
		t.Fatalf("a denied call must be a warning, not a failure: %+v %v", denied, err)
	}
	for name, s := range bad {
		if _, err := parseClaudeStream(s); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

func TestClaudeArgsConfineReviewer(t *testing.T) {
	args := strings.Join(claudeArgs("m", "low", findingsSchema), " ")
	for _, want := range []string{"--restricted", "--tools Read,Grep,Glob", "--strict-mcp-config", "--no-session-persistence", "--effort low", "--json-schema"} {
		if !strings.Contains(args, want) {
			t.Errorf("args missing %q", want)
		}
	}
	if strings.Contains(args, "Bash") || strings.Contains(args, "--mcp-config") {
		t.Errorf("args must not enable Bash or MCP: %s", args)
	}
}

// fakeRunner answers each pass: defect findings by effort, trigger checks by name, structure and judge outputs.
type fakeRunner struct {
	byEffort  map[string][]Finding
	checks    map[string]checkOutput
	structure []Finding
	judge     []JudgeMatch
	mu        sync.Mutex
	calls     []string // defect pass efforts, in order
	passes    []string
	workDirs  []string
	sawPrior  bool
}

func (f *fakeRunner) Model() string { return "fake" }

func (f *fakeRunner) Call(_ context.Context, c Call) (CallResult, error) {
	f.mu.Lock()
	f.passes = append(f.passes, c.Pass)
	f.workDirs = append(f.workDirs, c.WorkDir)
	if c.Pass == PassDefect {
		f.calls = append(f.calls, c.Effort)
		if _, err := os.Stat(filepath.Join(c.WorkDir, "prior_findings.md")); err == nil {
			f.sawPrior = true
		}
	}
	f.mu.Unlock()
	if c.Pass != "judge" {
		for _, name := range []string{"issue.md", "change.diff", "repo", "facts.md"} {
			if _, err := os.Stat(filepath.Join(c.WorkDir, name)); err != nil {
				return CallResult{}, err
			}
		}
	}
	var out any
	switch {
	case c.Pass == PassDefect:
		out = Output{Findings: f.byEffort[c.Effort], InspectedFiles: []string{}}
	case c.Pass == PassStructure:
		out = Output{Findings: f.structure, InspectedFiles: []string{}}
	case c.Pass == "judge":
		out = map[string]any{"matches": f.judge}
	case strings.HasPrefix(c.Pass, "trigger:"):
		co, ok := f.checks[strings.TrimPrefix(c.Pass, "trigger:")]
		if !ok {
			co = checkOutput{Targets: []string{"t"}, Conclusion: "no_issue", Evidence: []string{"repo/x:1"}}
		}
		out = co
	}
	data, _ := json.Marshal(out)
	return CallResult{Output: data, ObservedModel: "fake", Duration: time.Second}, nil
}

func newService(t *testing.T, r Runner) *Service {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "review.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return &Service{Store: s, Runner: r, WorkRoot: t.TempDir()}
}

func TestServiceEscalatesAndRecordsVerdicts(t *testing.T) {
	repo, a, b := newRepo(t)
	runner := &fakeRunner{byEffort: map[string][]Finding{
		"low":  {{Title: "maybe", Severity: "high", Confidence: "low", File: "app/a.rb", Line: 1}},
		"high": {{Title: "real", Severity: "high", Confidence: "high", File: "app/a.rb", Line: 1}, {Title: "minor", Severity: "low", Confidence: "medium", File: "app/a.rb", Line: 2}},
	}}
	svc := newService(t, runner)
	ctx := context.Background()
	rep, err := svc.Run(ctx, &CommitRange{Repo: repo, Base: a, Head: b}, Options{Effort: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(runner.calls, ",") != "low,high" || len(rep.Earlier) != 1 || rep.Run.EscalatedFrom != rep.Earlier[0].ID || len(rep.Run.Findings) != 2 {
		t.Fatalf("calls=%v report=%+v", runner.calls, rep)
	}
	if _, err := os.Stat(runner.workDirs[0]); !os.IsNotExist(err) {
		t.Fatal("workspace must be removed after the run")
	}
	id := rep.Run.Findings[0].ID
	if _, err := svc.Store.Judge(ctx, id, "오탐", "first", "me"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store.Judge(ctx, id, "defect", "second look", "me"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store.Judge(ctx, id, "wrong", "", "me"); err == nil {
		t.Fatal("invalid verdict accepted")
	}
	got, err := svc.Store.GetRun(ctx, rep.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Findings[0].Verdict == nil || got.Findings[0].Verdict.Verdict != "defect" || got.Findings[1].Verdict != nil || got.Target["head"] != b {
		t.Fatalf("latest verdict not reported: %+v", got.Findings)
	}
	if got.Findings[0].Origin != "model" {
		t.Fatalf("model findings must be marked: %+v", got.Findings[0])
	}
	missed, err := svc.Store.AddFinding(ctx, rep.Run.ID, "human", Finding{Title: "missed", Severity: "high", Confidence: "high", File: "app/a.rb", Line: 9}, "", "me", "")
	if err != nil || missed.ID != rep.Run.ID[:8]+"-3" || missed.Verdict == nil || missed.Verdict.Verdict != "defect" {
		t.Fatalf("human finding must continue ordinals and carry a defect verdict: %+v %v", missed, err)
	}
	agent, err := svc.Store.AddFinding(ctx, rep.Run.ID, "agent", Finding{Title: "agent", Severity: "low", Confidence: "medium", File: "app/a.rb", Line: 2}, "", "me", "")
	if err != nil || agent.Verdict != nil {
		t.Fatalf("agent finding must wait for a verdict: %+v %v", agent, err)
	}
	for name, bad := range map[string]func() error{
		"run": func() error {
			_, e := svc.Store.AddFinding(ctx, "missing", "human", Finding{Title: "x", Severity: "low", Confidence: "low", File: "f", Line: 1}, "", "me", "")
			return e
		},
		"origin": func() error {
			_, e := svc.Store.AddFinding(ctx, rep.Run.ID, "model", Finding{Title: "x", Severity: "low", Confidence: "low", File: "f", Line: 1}, "", "me", "")
			return e
		},
		"severity": func() error {
			_, e := svc.Store.AddFinding(ctx, rep.Run.ID, "human", Finding{Title: "x", Severity: "max", Confidence: "low", File: "f", Line: 1}, "", "me", "")
			return e
		},
	} {
		if bad() == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
	for name, bad := range map[string]func() error{
		"human unsure": func() error {
			_, e := svc.Store.AddFinding(ctx, rep.Run.ID, "human", Finding{Title: "x", Severity: "low", Confidence: "low", File: "f", Line: 1}, "미확정", "me", "")
			return e
		},
		"agent verdict": func() error {
			_, e := svc.Store.AddFinding(ctx, rep.Run.ID, "agent", Finding{Title: "x", Severity: "low", Confidence: "low", File: "f", Line: 1}, "useful", "me", "")
			return e
		},
	} {
		if bad() == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
	good, err := svc.Store.AddFinding(ctx, rep.Run.ID, "human", Finding{Title: "nice", Severity: "low", Confidence: "high", File: "app/a.rb", Line: 3}, "유용", "me", "결함은 아닌데 좋은 지적")
	if err != nil || good.Verdict.Verdict != "useful" {
		t.Fatalf("useful human finding: %+v %v", good, err)
	}
	if v, err := svc.Store.Judge(ctx, agent.ID, "유용", "", "me"); err != nil || v.Verdict != "useful" {
		t.Fatalf("judge useful: %+v %v", v, err)
	}
	got, _ = svc.Store.GetRun(ctx, rep.Run.ID)
	if len(got.Findings) != 5 || got.Findings[2].Origin != "human" || got.Findings[3].Origin != "agent" {
		t.Fatalf("added findings not listed: %+v", got.Findings)
	}
	list, err := svc.Store.ListRuns(ctx, 10)
	if err != nil || len(list) != 2 || list[0].Judged+list[1].Judged != 4 {
		t.Fatalf("list=%+v err=%v", list, err)
	}

	runner.calls = nil
	if _, err := svc.Run(ctx, &CommitRange{Repo: repo, Base: a, Head: b}, Options{Effort: "low"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(runner.calls, ",") != "low" {
		t.Fatalf("explicit effort must not escalate: %v", runner.calls)
	}
}

func TestEvalCountsNearHitsAndKeepsHistory(t *testing.T) {
	repo, a, b := newRepo(t)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "issue.md"), "goal")
	suite := Suite{Name: "s", Repo: repo, Cases: []SuiteCase{
		{ID: "D-1", Kind: "defect", Base: a, Head: b, IssueFile: "issue.md", Expected: []Expected{{File: "/app/a.rb", Line: 20}, {File: "app/z.rb", Line: 1}}},
		{ID: "N-1", Kind: "normal", Base: a, Head: b},
	}}
	runner := &fakeRunner{byEffort: map[string][]Finding{"low": {{Title: "x", Severity: "medium", File: "repo/app/a.rb", Line: 1}}},
		judge: []JudgeMatch{{Expected: 1, Verdict: "same_cause"}, {Expected: 2, Verdict: "partial"}}}
	svc := newService(t, runner)
	res, prev, err := svc.Eval(context.Background(), suite, dir, Options{Effort: "low"}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if prev != nil || res.NearHits != 1 || res.Matched != 1 || res.Partial != 1 || res.Expected != 2 || res.NormalFindings != 1 || res.Failed != 0 || res.Incomplete != 0 {
		t.Fatalf("result=%+v", res)
	}
	_, prev, err = svc.Eval(context.Background(), suite, dir, Options{Effort: "low"}, 2)
	if err != nil || prev == nil {
		t.Fatalf("previous eval not returned: %v", err)
	}
}

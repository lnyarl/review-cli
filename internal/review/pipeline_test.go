package review

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const triggerDiff = `diff --git a/app/models/capture.rb b/app/models/capture.rb
--- a/app/models/capture.rb
+++ b/app/models/capture.rb
@@ -10,6 +10,8 @@ class Capture
-    return if pending?
-  def old_helper
+    with_lock do
+      Worker.perform_at(30.seconds.from_now, id)
+      raise Errors::Conflict if busy?
+      n = Integer(params[:count]) # rubocop:disable Style/Foo
     end
diff --git a/spec/models/capture_spec.rb b/spec/models/capture_spec.rb
new file mode 100644
--- /dev/null
+++ b/spec/models/capture_spec.rb
@@ -0,0 +1,2 @@
+it 'locks' do
+  with_lock
`

func TestTriggersAndDeterministicChecks(t *testing.T) {
	fds := parseDiff([]byte(triggerDiff))
	if len(fds) != 2 || !fds[1].New || fds[0].Added[0].N != 10 || fds[0].Removed[1].Text != "  def old_helper" {
		t.Fatalf("parsed %+v", fds)
	}
	var names []string
	for _, tr := range detectTriggers(fds) {
		names = append(names, tr.Name)
		for _, l := range tr.Locations {
			if strings.Contains(l, "spec/") {
				t.Errorf("%s must ignore test files: %s", tr.Name, l)
			}
		}
	}
	sort.Strings(names)
	want := "async_hop,exception_status,guard_removed,input_parsing,lock_reload,removed_contract"
	if strings.Join(names, ",") != want {
		t.Fatalf("triggers = %v, want %s", names, want)
	}
	c, fs := suppressionCheck(fds)
	if c.Conclusion != "issue" || len(fs) != 1 || fs[0].Origin != "check" || fs[0].Line != 13 || fs[0].Category != "structure" {
		t.Fatalf("suppression = %+v %+v", c, fs)
	}

	work := t.TempDir()
	write(t, filepath.Join(work, "repo", "app", "jobs", "a.rb"), "x.old_helper\n")
	write(t, filepath.Join(work, "repos", "agent", "src", "b.ts"), "call(old_helper)\n")
	c, fs = removedDefinitionCheck(work, fds)
	if c.Conclusion != "issue" || len(fs) != 1 || !strings.Contains(fs[0].CodePath, "repos/agent/src/b.ts:1") || !strings.Contains(fs[0].CodePath, "repo/app/jobs/a.rb:1") {
		t.Fatalf("removed definition = %+v %+v", c, fs)
	}

	codeDir := filepath.Join(work, "repo")
	write(t, filepath.Join(codeDir, "lib", "tasks", "test.rake"), "FileList['spec/lib/**/*_spec.rb']\n")
	tr, ok := newTestsTrigger(codeDir, fds)
	if !ok || !strings.Contains(tr.Locations[0], "spec/models/capture_spec.rb") || !strings.Contains(tr.Locations[0], "없음") ||
		!strings.Contains(tr.Locations[1], "lib/tasks/test.rake") {
		t.Fatalf("new tests trigger = %+v", tr)
	}
}

func TestMergeCheckFindsConflicts(t *testing.T) {
	repo, a, b := newRepo(t)
	gitT(t, repo, "checkout", "-q", "-b", "other", a)
	write(t, filepath.Join(repo, "app", "a.rb"), "other\n")
	gitT(t, repo, "commit", "-qam", "other")
	write(t, filepath.Join(repo, "keep.rb"), "keep2\n")
	gitT(t, repo, "checkout", "-q", "-b", "clean", a)
	write(t, filepath.Join(repo, "keep.rb"), "keep2\n")
	gitT(t, repo, "commit", "-qam", "clean")
	c, fs := mergeCheck(context.Background(), repo, b, []string{"other", "clean"})
	if c.Conclusion != "issue" || len(fs) != 1 || fs[0].File != "app/a.rb" || !strings.Contains(strings.Join(c.Evidence, "|"), "clean: 충돌 없음") {
		t.Fatalf("merge check = %+v %+v", c, fs)
	}
	if c, _ := mergeCheck(context.Background(), repo, "", []string{"other"}); c.Conclusion != "not_run" {
		t.Fatalf("no head must not run: %+v", c)
	}
}

// lockRepo returns a repo whose second commit adds a with_lock call, so the lock_reload trigger fires.
func lockRepo(t *testing.T) (repo, a, b string) {
	repo = t.TempDir()
	gitT(t, repo, "init", "-q", "-b", "main")
	gitT(t, repo, "remote", "add", "origin", "git@example:org/proj/lock")
	write(t, filepath.Join(repo, "app", "c.rb"), "def run\nend\n")
	gitT(t, repo, "add", ".")
	gitT(t, repo, "commit", "-qm", "A")
	a = gitT(t, repo, "rev-parse", "HEAD")
	write(t, filepath.Join(repo, "app", "c.rb"), "def run\n  with_lock { save! } # rubocop:disable Foo\nend\n")
	gitT(t, repo, "commit", "-qam", "B")
	return repo, a, gitT(t, repo, "rev-parse", "HEAD")
}

func TestPipelineRecordsPassesAndCompleteness(t *testing.T) {
	repo, a, b := lockRepo(t)
	runner := &fakeRunner{
		byEffort:  map[string][]Finding{"low": {{Title: "d", Severity: "medium", Confidence: "high", File: "app/c.rb", Line: 2, Scope: "out_of_scope"}}},
		checks:    map[string]checkOutput{"lock_reload": {Targets: []string{"app/c.rb"}, Conclusion: "issue", Evidence: []string{"repo/app/c.rb:2"}, Findings: []Finding{{Title: "alias lost", Severity: "medium", Confidence: "medium", File: "app/c.rb", Line: 2}}}},
		structure: []Finding{{Title: "layer", Severity: "low", Confidence: "medium", File: "app/c.rb", Line: 1}},
	}
	svc := newService(t, runner)
	ctx := context.Background()
	rep, err := svc.Run(ctx, &CommitRange{Repo: repo, Base: a, Head: b}, Options{Effort: "auto"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.Store.GetRun(ctx, rep.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	passes := map[string]StoredFinding{}
	for _, f := range got.Findings {
		passes[f.Pass] = f
	}
	if !got.Complete || passes["defect"].Scope != "out_of_scope" || passes["trigger:lock_reload"].Title != "alias lost" ||
		passes["structure"].Category != "structure" || passes["check:suppression"].Origin != "check" || len(got.Findings) != 4 {
		t.Fatalf("findings = %+v complete=%v", got.Findings, got.Complete)
	}
	var lock *Check
	for i := range got.Checks {
		if got.Checks[i].Name == "lock_reload" {
			lock = &got.Checks[i]
		}
	}
	if lock == nil || lock.Conclusion != "issue" || got.Reviewed["app/c.rb"] == "" {
		t.Fatalf("checks = %+v reviewed=%v", got.Checks, got.Reviewed)
	}

	runner.checks["lock_reload"] = checkOutput{Targets: []string{"app/c.rb"}, Conclusion: "no_issue"}
	rep, err = svc.Run(ctx, &CommitRange{Repo: repo, Base: a, Head: b}, Options{Effort: "low"})
	if err != nil || rep.Run.Complete {
		t.Fatalf("a check without evidence must make the run incomplete: %+v %v", rep.Run.Checks, err)
	}
	if !runner.sawPrior {
		t.Fatal("the second review must receive prior findings on the same file")
	}
	runner.sawPrior = false
	if _, err := svc.Run(ctx, &CommitRange{Repo: repo, Base: a, Head: b}, Options{Effort: "low", PriorBefore: "2000-01-01T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if runner.sawPrior {
		t.Fatal("findings of runs after the cutoff must not be carried forward")
	}
	runner.passes = nil
	if _, err := svc.Run(ctx, &CommitRange{Repo: repo, Base: a, Head: b}, Options{Effort: "low", Passes: []string{PassDefect}}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(runner.passes, ",") != "defect" {
		t.Fatalf("--passes defect must run only the defect pass: %v", runner.passes)
	}
}

func TestCoverageFollowsReviewedContent(t *testing.T) {
	repo, a, b := newRepo(t)
	svc := newService(t, &fakeRunner{})
	ctx := context.Background()
	if _, err := svc.Run(ctx, &CommitRange{Repo: repo, Base: a, Head: b}, Options{Effort: "low"}); err != nil {
		t.Fatal(err)
	}
	c, err := ComputeCoverage(ctx, svc.Store, repo, a, b)
	if err != nil || !c.AllCovered || c.Covered != 1 {
		t.Fatalf("reviewed range must be covered: %+v %v", c, err)
	}
	write(t, filepath.Join(repo, "app", "a.rb"), "v3\n")
	write(t, filepath.Join(repo, "new.rb"), "n\n")
	gitT(t, repo, "add", ".")
	gitT(t, repo, "commit", "-qm", "C")
	cHead := gitT(t, repo, "rev-parse", "HEAD")
	c, err = ComputeCoverage(ctx, svc.Store, repo, a, cHead)
	if err != nil || c.AllCovered || c.Uncovered != 2 || c.Suggestion == "" {
		t.Fatalf("files changed after review must be uncovered: %+v %v", c, err)
	}
	if _, err := svc.Run(ctx, &CommitRange{Repo: repo, Base: b, Head: cHead}, Options{Effort: "low", Purpose: "targeted"}); err != nil {
		t.Fatal(err)
	}
	if c, _ = ComputeCoverage(ctx, svc.Store, repo, a, cHead); c.AllCovered {
		t.Fatal("a targeted run must not count as coverage")
	}
	patch := gitT(t, repo, "diff", "--binary", b, cHead) + "\n"
	if _, err := svc.Run(ctx, &Patch{Repo: repo, Base: b, Diff: []byte(patch)}, Options{Effort: "low"}); err != nil {
		t.Fatal(err)
	}
	if c, _ = ComputeCoverage(ctx, svc.Store, repo, a, cHead); !c.AllCovered {
		t.Fatalf("a patch review of the same content must count: %+v", c)
	}
}

func TestContextFilesAndSiblingRepos(t *testing.T) {
	repo, a, b := newRepo(t)
	sibling, _, _ := newRepo(t)
	docs := t.TempDir()
	write(t, filepath.Join(docs, "epic.md"), "contract")
	write(t, filepath.Join(docs, "img.bin"), "\x00\x01")
	keep := &fakeRunner{}
	svc := newService(t, keep)
	svc.KeepWorkspace = true
	rep, err := svc.Run(context.Background(), &CommitRange{Repo: repo, Base: a, Head: b},
		Options{Effort: "low", Context: []string{docs}, Repos: []RepoRef{{Name: "agent", Path: sibling}}})
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(rep.Workspace)
	if read(t, filepath.Join(rep.Workspace, "context", filepath.Base(docs), "epic.md")) != "contract" ||
		read(t, filepath.Join(rep.Workspace, "repos", "agent", "app", "a.rb")) != "v2\n" {
		t.Fatal("context and sibling repo must be in the workspace")
	}
	if _, err := os.Stat(filepath.Join(rep.Workspace, "context", filepath.Base(docs), "img.bin")); !os.IsNotExist(err) {
		t.Fatal("binary context files must be skipped")
	}
	if !strings.HasPrefix(rep.Run.Target["with_repos"], "agent@") || !strings.Contains(read(t, filepath.Join(rep.Workspace, "facts.md")), "epic.md") {
		t.Fatalf("target=%v", rep.Run.Target)
	}
	if _, err := ParseRepoRef("bad name=/x"); err == nil {
		t.Fatal("invalid repo name accepted")
	}
	if r, err := ParseRepoRef("agent=/p@v1"); err != nil || r.Rev != "v1" || r.Path != "/p" {
		t.Fatalf("parse = %+v %v", r, err)
	}
}

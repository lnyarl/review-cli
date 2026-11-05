package review

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Material is what a reviewer reads: code exported without .git, the change diff and the issue text.
type Material struct {
	CodeDir string
	Diff    []byte
	Issue   string
	// Repo and Head locate the reviewed commit for checks that need git (Head is empty for patches and working trees).
	Repo, Head string
}

// Target is one kind of review input. New inputs add an implementation; the service does not change.
type Target interface {
	Kind() string
	// Describe identifies the input for the ledger. It is called after Prepare.
	Describe() map[string]string
	Prepare(ctx context.Context, codeDir string) (Material, error)
}

// CommitRange reviews base...head of a local repository.
type CommitRange struct {
	Repo, Base, Head, Issue    string
	resolvedBase, resolvedHead string
}

func (t *CommitRange) Kind() string { return "commit_range" }

func (t *CommitRange) Describe() map[string]string {
	return map[string]string{"repo": t.Repo, "remote": remoteURL(context.Background(), t.Repo), "base": t.resolvedBase, "head": t.resolvedHead}
}

func (t *CommitRange) Prepare(ctx context.Context, codeDir string) (Material, error) {
	var err error
	if t.resolvedBase, err = resolveCommit(ctx, t.Repo, t.Base); err != nil {
		return Material{}, err
	}
	if t.resolvedHead, err = resolveCommit(ctx, t.Repo, t.Head); err != nil {
		return Material{}, err
	}
	if err := exportCommit(ctx, t.Repo, t.resolvedHead, codeDir); err != nil {
		return Material{}, err
	}
	diff, err := git(ctx, t.Repo, "diff", t.resolvedBase+"..."+t.resolvedHead)
	if err != nil {
		return Material{}, err
	}
	return Material{CodeDir: codeDir, Diff: diff, Issue: t.Issue, Repo: t.Repo, Head: t.resolvedHead}, nil
}

// Patch reviews any change a caller can express as a patch against a base commit (staged changes,
// a subset of files, a proposed edit). The code is base with the patch applied; the diff is the patch.
// Label records which scope the caller meant, e.g. "staged".
type Patch struct {
	Repo, Base, Label, Issue string
	Diff                     []byte
	resolvedBase             string
	files                    []string
}

func (t *Patch) Kind() string { return "patch" }

func (t *Patch) Describe() map[string]string {
	sum := sha256.Sum256(t.Diff)
	return map[string]string{"repo": t.Repo, "remote": remoteURL(context.Background(), t.Repo), "base": t.resolvedBase,
		"label": t.Label, "patch_sha256": hex.EncodeToString(sum[:]), "files": strings.Join(t.files, ",")}
}

func (t *Patch) Prepare(ctx context.Context, codeDir string) (Material, error) {
	if len(bytes.TrimSpace(t.Diff)) == 0 {
		return Material{}, fmt.Errorf("empty patch")
	}
	var err error
	if t.resolvedBase, err = resolveCommit(ctx, t.Repo, t.Base); err != nil {
		return Material{}, err
	}
	if err := exportCommit(ctx, t.Repo, t.resolvedBase, codeDir); err != nil {
		return Material{}, err
	}
	// git apply works outside a repository, so the export stays free of .git. The ceiling stops git from
	// adopting an enclosing repository when the workspace root happens to live inside one.
	cmd := exec.CommandContext(ctx, "git", "apply", "--whitespace=nowarn", "-")
	cmd.Dir, cmd.Stdin = codeDir, bytes.NewReader(t.Diff)
	cmd.Env = append(os.Environ(), "GIT_CEILING_DIRECTORIES="+filepath.Dir(codeDir))
	if out, err := cmd.CombinedOutput(); err != nil {
		return Material{}, fmt.Errorf("patch does not apply to %s: %s", t.resolvedBase, strings.TrimSpace(string(out)))
	}
	for _, f := range diffFile.FindAllStringSubmatch(string(t.Diff), -1) {
		t.files = append(t.files, f[1])
	}
	return Material{CodeDir: codeDir, Diff: t.Diff, Issue: t.Issue, Repo: t.Repo}, nil
}

// WorkingTree reviews uncommitted changes (tracked and untracked) against HEAD.
// Paths, when set, limits the review to those files.
type WorkingTree struct {
	Repo  string
	Paths []string
	Issue string
	head  string
	files []string
}

func (t *WorkingTree) Kind() string { return "working_tree" }

func (t *WorkingTree) Describe() map[string]string {
	return map[string]string{"repo": t.Repo, "remote": remoteURL(context.Background(), t.Repo), "head": t.head, "files": strings.Join(t.files, ",")}
}

func (t *WorkingTree) Prepare(ctx context.Context, codeDir string) (Material, error) {
	var err error
	if t.head, err = resolveCommit(ctx, t.Repo, "HEAD"); err != nil {
		return Material{}, err
	}
	if err := exportCommit(ctx, t.Repo, t.head, codeDir); err != nil {
		return Material{}, err
	}
	status, err := git(ctx, t.Repo, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return Material{}, err
	}
	want := map[string]bool{}
	for _, p := range t.Paths {
		want[filepath.Clean(p)] = true
	}
	var diff bytes.Buffer
	entries := strings.Split(string(status), "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) < 4 {
			continue
		}
		code, path := e[:2], e[3:]
		renamedFrom := ""
		if (code[0] == 'R' || code[0] == 'C') && i+1 < len(entries) {
			i++ // with -z the source path follows as its own entry
			if code[0] == 'R' {
				renamedFrom = entries[i]
			}
		}
		if len(want) > 0 && !want[filepath.Clean(path)] {
			continue
		}
		if renamedFrom != "" {
			if err := os.Remove(filepath.Join(codeDir, renamedFrom)); err != nil && !os.IsNotExist(err) {
				return Material{}, err
			}
		}
		t.files = append(t.files, path)
		src, dst := filepath.Join(t.Repo, path), filepath.Join(codeDir, path)
		info, err := os.Lstat(src)
		switch {
		case os.IsNotExist(err):
			if err := os.Remove(dst); err != nil && !os.IsNotExist(err) {
				return Material{}, err
			}
		case err != nil:
			return Material{}, err
		case info.Mode().IsRegular():
			if err := copyFile(src, dst); err != nil {
				return Material{}, err
			}
		}
		var d []byte
		if code == "??" {
			d, err = gitDiffNoIndex(ctx, t.Repo, path)
		} else if renamedFrom != "" {
			d, err = git(ctx, t.Repo, "diff", "-M", "HEAD", "--", renamedFrom, path)
		} else {
			d, err = git(ctx, t.Repo, "diff", "HEAD", "--", path)
		}
		if err != nil {
			return Material{}, err
		}
		diff.Write(d)
	}
	if len(want) > 0 && len(t.files) != len(want) {
		return Material{}, fmt.Errorf("some requested paths have no uncommitted changes: requested %d, found %d", len(want), len(t.files))
	}
	if diff.Len() == 0 {
		return Material{}, fmt.Errorf("no uncommitted changes to review")
	}
	return Material{CodeDir: codeDir, Diff: diff.Bytes(), Issue: t.Issue, Repo: t.Repo}, nil
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// AdoPR resolves an Azure DevOps PR from the collector DB (read-only) to its last merge target/source
// commits. The PR repository's sshUrl must equal the local repository's origin (no name matching).
type AdoPR struct {
	CollectorDB string
	Repo        string
	PRID        int
	Issue       string
	inner       *CommitRange
	title       string
}

func (t *AdoPR) Kind() string { return "ado_pr" }

func (t *AdoPR) Describe() map[string]string {
	d := map[string]string{"pr_id": fmt.Sprint(t.PRID), "title": t.title}
	if t.inner != nil {
		for k, v := range t.inner.Describe() {
			d[k] = v
		}
	}
	return d
}

func (t *AdoPR) Prepare(ctx context.Context, codeDir string) (Material, error) {
	pr, err := readCollectorPR(ctx, t.CollectorDB, t.PRID)
	if err != nil {
		return Material{}, err
	}
	if remote := remoteURL(ctx, t.Repo); remote == "" || remote != pr.Repository.SSHURL {
		return Material{}, fmt.Errorf("unresolved_repository: PR %d belongs to %q, local origin is %q", t.PRID, pr.Repository.SSHURL, remote)
	}
	if pr.LastMergeTargetCommit.CommitID == "" || pr.LastMergeSourceCommit.CommitID == "" {
		return Material{}, fmt.Errorf("unresolved_commit: PR %d has no merge source/target commit", t.PRID)
	}
	t.title = pr.Title
	issue := t.Issue
	if issue == "" {
		issue = "# PR " + fmt.Sprint(t.PRID) + ": " + pr.Title + "\n\n" + pr.Description
	}
	t.inner = &CommitRange{Repo: t.Repo, Base: pr.LastMergeTargetCommit.CommitID, Head: pr.LastMergeSourceCommit.CommitID, Issue: issue}
	return t.inner.Prepare(ctx, codeDir)
}

type collectorPR struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Repository  struct {
		SSHURL string `json:"sshUrl"`
	} `json:"repository"`
	LastMergeSourceCommit struct {
		CommitID string `json:"commitId"`
	} `json:"lastMergeSourceCommit"`
	LastMergeTargetCommit struct {
		CommitID string `json:"commitId"`
	} `json:"lastMergeTargetCommit"`
}

func readCollectorPR(ctx context.Context, path string, id int) (collectorPR, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return collectorPR{}, err
	}
	u := url.URL{Scheme: "file", Path: abs}
	q := u.Query()
	q.Set("mode", "ro")
	q.Add("_pragma", "query_only(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return collectorPR{}, err
	}
	defer db.Close()
	var raw string
	err = db.QueryRowContext(ctx, `SELECT event_json FROM raw_items WHERE provider='azuredevops' AND external_id LIKE ? ORDER BY id DESC LIMIT 1`, fmt.Sprintf("%%:pr:%d", id)).Scan(&raw)
	if err == sql.ErrNoRows {
		return collectorPR{}, fmt.Errorf("PR %d is not in the collector DB", id)
	}
	if err != nil {
		return collectorPR{}, err
	}
	var event struct {
		Payload struct {
			PullRequest collectorPR `json:"pull_request"`
		} `json:"payload"`
	}
	if err := json.Unmarshal([]byte(raw), &event); err != nil {
		return collectorPR{}, err
	}
	return event.Payload.PullRequest, nil
}

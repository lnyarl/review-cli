package review

import (
	"context"
	"fmt"
	"strings"
)

type CoverageFile struct {
	Path      string `json:"path"`
	Blob      string `json:"blob"`
	CoveredBy string `json:"covered_by,omitempty"`
	CoveredAt string `json:"covered_at,omitempty"`
}

// Coverage tells which files of base...head have been reviewed in exactly their head content. A file changed
// again after its last review is uncovered, so fix commits pushed after a review are not mistaken for reviewed.
type Coverage struct {
	Repo       string         `json:"repo"`
	Remote     string         `json:"remote"`
	MergeBase  string         `json:"merge_base"`
	Head       string         `json:"head"`
	Files      []CoverageFile `json:"files"`
	Covered    int            `json:"covered"`
	Uncovered  int            `json:"uncovered"`
	AllCovered bool           `json:"all_covered"`
	Suggestion string         `json:"suggestion,omitempty"`
}

func ComputeCoverage(ctx context.Context, store *Store, repo, base, head string) (Coverage, error) {
	remote := remoteURL(ctx, repo)
	if remote == "" {
		return Coverage{}, fmt.Errorf("repository %s has no origin remote; coverage matches runs by remote", repo)
	}
	h, err := resolveCommit(ctx, repo, head)
	if err != nil {
		return Coverage{}, err
	}
	b, err := resolveCommit(ctx, repo, base)
	if err != nil {
		return Coverage{}, err
	}
	out, err := git(ctx, repo, "merge-base", b, h)
	if err != nil {
		return Coverage{}, err
	}
	mb := strings.TrimSpace(string(out))
	names, err := git(ctx, repo, "diff", "--name-only", "-z", "--no-renames", mb, h)
	if err != nil {
		return Coverage{}, err
	}
	cache := map[string]string{}
	blobAt := func(rev, path string) string {
		key := rev + ":" + path
		if v, ok := cache[key]; ok {
			return v
		}
		v := ""
		if o, err := git(ctx, repo, "rev-parse", "--verify", "-q", key); err == nil {
			v = strings.TrimSpace(string(o))
		}
		cache[key] = v
		return v
	}
	runs, err := store.CoverageRuns(ctx, remote, blobAt)
	if err != nil {
		return Coverage{}, err
	}
	c := Coverage{Repo: repo, Remote: remote, MergeBase: mb, Head: h, Files: []CoverageFile{}}
	for _, p := range strings.Split(strings.TrimRight(string(names), "\x00"), "\x00") {
		if p == "" {
			continue
		}
		f := CoverageFile{Path: p, Blob: blobAt(h, p)}
		if f.Blob == "" {
			f.Blob = "deleted"
		}
		for i := len(runs) - 1; i >= 0; i-- {
			if runs[i].Reviewed[p] == f.Blob {
				f.CoveredBy, f.CoveredAt = runs[i].ID, runs[i].CreatedAt
				break
			}
		}
		if f.CoveredBy == "" {
			c.Uncovered++
		} else {
			c.Covered++
		}
		c.Files = append(c.Files, f)
	}
	c.AllCovered = c.Uncovered == 0
	if !c.AllCovered {
		c.Suggestion = fmt.Sprintf("review run --repo %s --base %s --head %s", repo, mb, h)
	}
	return c, nil
}

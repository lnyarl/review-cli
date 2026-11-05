package review

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// RepoRef is a sibling repository exported read-only under ./repos/<Name> so contracts across repos can be read.
type RepoRef struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Rev  string `json:"rev,omitempty"`
}

var repoName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ParseRepoRef reads name=path[@rev].
func ParseRepoRef(s string) (RepoRef, error) {
	name, rest, ok := strings.Cut(s, "=")
	if !ok || !repoName.MatchString(name) || rest == "" {
		return RepoRef{}, fmt.Errorf("--with-repo must be name=path[@rev], got %q", s)
	}
	path, rev, _ := strings.Cut(rest, "@")
	return RepoRef{Name: name, Path: path, Rev: rev}, nil
}

func writeWorkspace(workDir string, m Material) error {
	if err := os.WriteFile(filepath.Join(workDir, "change.diff"), m.Diff, 0o600); err != nil {
		return err
	}
	issue := m.Issue
	if strings.TrimSpace(issue) == "" {
		issue = "(이슈 설명 없음)"
	}
	return os.WriteFile(filepath.Join(workDir, "issue.md"), []byte(issue), 0o600)
}

// exportRepos exports each sibling repository at its revision (default HEAD) without .git and returns name@commit.
func exportRepos(ctx context.Context, work string, refs []RepoRef) ([]string, error) {
	var out []string
	for _, r := range refs {
		rev := r.Rev
		if rev == "" {
			rev = "HEAD"
		}
		commit, err := resolveCommit(ctx, r.Path, rev)
		if err != nil {
			return nil, fmt.Errorf("--with-repo %s: %w", r.Name, err)
		}
		dst := filepath.Join(work, "repos", r.Name)
		if err := os.MkdirAll(dst, 0o700); err != nil {
			return nil, err
		}
		if err := exportCommit(ctx, r.Path, commit, dst); err != nil {
			return nil, err
		}
		out = append(out, r.Name+"@"+commit)
	}
	return out, nil
}

const maxContextBytes = 20 << 20

// copyContext copies --context-file files and directories (text only) into ./context.
func copyContext(work string, paths []string) ([]string, error) {
	var copied []string
	total := 0
	for _, src := range paths {
		info, err := os.Stat(src)
		if err != nil {
			return nil, fmt.Errorf("--context-file: %w", err)
		}
		base := filepath.Base(filepath.Clean(src))
		err = filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if p != src && (strings.HasPrefix(d.Name(), ".") || skipDir(d.Name())) {
					return filepath.SkipDir
				}
				return nil
			}
			rel := base
			if info.IsDir() {
				r, _ := filepath.Rel(src, p)
				rel = filepath.Join(base, r)
			}
			data, err := os.ReadFile(p)
			if err != nil || !isText(data) {
				return nil
			}
			if total += len(data); total > maxContextBytes {
				return fmt.Errorf("--context-file: more than %d MiB of text", maxContextBytes>>20)
			}
			dst := filepath.Join(work, "context", rel)
			if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
				return err
			}
			copied = append(copied, filepath.ToSlash(rel))
			return os.WriteFile(dst, data, 0o600)
		})
		if err != nil {
			return nil, err
		}
	}
	return copied, nil
}

func isText(data []byte) bool {
	n := len(data)
	if n > 8000 {
		n = 8000
	}
	for _, b := range data[:n] {
		if b == 0 {
			return false
		}
	}
	return true
}

func writePriorFindings(w io.Writer, prior []PriorFinding) {
	fmt.Fprintln(w, "# 이전 리뷰 지적")
	fmt.Fprintln(w)
	for _, p := range prior {
		v := p.Verdict
		if v == "" {
			v = "판정 없음"
		}
		fmt.Fprintf(w, "- [%s] %s:%d %s (판정: %s, %s)\n  조건: %s\n  영향: %s\n", p.ID, p.File, p.Line, p.Title, v, p.CreatedAt[:10],
			truncate(p.Condition, 400), truncate(p.Impact, 300))
	}
}

func writeFacts(w io.Writer, f Facts, repos, context, consumers, structure []string) {
	fmt.Fprintln(w, "# 미리 수집한 사실")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "## 트리거(점검 대상 위치)")
	if len(f.Triggers) == 0 {
		fmt.Fprintln(w, "- 없음")
	}
	for _, t := range f.Triggers {
		fmt.Fprintf(w, "- %s\n", t.Name)
		for _, l := range t.Locations {
			fmt.Fprintf(w, "  - %s\n", l)
		}
	}
	fmt.Fprintln(w, "\n## 결정적 점검")
	for _, c := range f.Checks {
		fmt.Fprintf(w, "- %s: %s %s\n", c.Name, c.Conclusion, c.NotChecked)
		for _, e := range c.Evidence {
			fmt.Fprintf(w, "  - %s\n", e)
		}
	}
	fmt.Fprintln(w, "\n## 연관 저장소(./repos)")
	if len(repos) == 0 {
		fmt.Fprintln(w, "- 없음")
	}
	for _, r := range repos {
		fmt.Fprintf(w, "- %s\n", r)
	}
	if len(consumers) > 0 {
		fmt.Fprintln(w, "\n## 연관 저장소에서 지운 키·정의를 쓰는 곳")
		for _, c := range consumers {
			fmt.Fprintf(w, "- %s\n", c)
		}
	}
	fmt.Fprintln(w, "\n## 추가 자료(./context)")
	if len(context) == 0 {
		fmt.Fprintln(w, "- 없음")
	}
	for _, c := range context {
		fmt.Fprintf(w, "- %s\n", c)
	}
	fmt.Fprintln(w, "\n## 구조")
	if len(structure) == 0 {
		fmt.Fprintln(w, "- 새 파일·중복 이름 없음")
	}
	for _, s := range structure {
		fmt.Fprintf(w, "- %s\n", s)
	}
}

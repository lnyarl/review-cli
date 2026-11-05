package review

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Facts are collected from the diff and the exported code before any model call. Triggers become narrow
// model checks; deterministic checks produce findings directly; Text is written to facts.md for every pass.
type Facts struct {
	Triggers []Trigger
	Checks   []Check
	Findings []StoredFinding
	Text     string
}

// Trigger is a diff pattern that has caused missed defects before, with the question that would have caught it.
type Trigger struct {
	Name      string
	Question  string
	Locations []string
}

type diffLine struct {
	N    int
	Text string
}

type fileDiff struct {
	Path, OldPath  string
	New, Deleted   bool
	Added, Removed []diffLine
}

var (
	hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)
	testPath   = regexp.MustCompile(`(^|/)(spec|test|tests|__tests__)/|_spec\.rb$|\.(spec|test)\.[jt]sx?$|_test\.go$|_test\.py$`)
)

func parseDiff(diff []byte) []fileDiff {
	var out []fileDiff
	var cur *fileDiff
	oldN, newN := 0, 0
	sc := bufio.NewScanner(bytes.NewReader(diff))
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "diff --git a/"):
			out = append(out, fileDiff{})
			cur = &out[len(out)-1]
			if i := strings.LastIndex(line, " b/"); i > 0 {
				cur.OldPath, cur.Path = strings.TrimPrefix(line[:i], "diff --git a/"), line[i+3:]
			}
		case cur == nil:
		case strings.HasPrefix(line, "new file mode"):
			cur.New = true
		case strings.HasPrefix(line, "deleted file mode"):
			cur.Deleted = true
		case strings.HasPrefix(line, "rename to "):
			cur.Path = strings.TrimPrefix(line, "rename to ")
		case strings.HasPrefix(line, "+++ ") || strings.HasPrefix(line, "--- "):
		case strings.HasPrefix(line, "@@"):
			if m := hunkHeader.FindStringSubmatch(line); m != nil {
				oldN, _ = strconv.Atoi(m[1])
				newN, _ = strconv.Atoi(m[2])
			}
		case strings.HasPrefix(line, "+"):
			cur.Added = append(cur.Added, diffLine{newN, line[1:]})
			newN++
		case strings.HasPrefix(line, "-"):
			cur.Removed = append(cur.Removed, diffLine{oldN, line[1:]})
			oldN++
		default:
			oldN++
			newN++
		}
	}
	return out
}

type triggerRule struct {
	name, question string
	added, removed *regexp.Regexp
}

// Each rule comes from a defect a human reviewer caught and the single-call review missed (2026-11 analysis).
var triggerRules = []triggerRule{
	{"lock_reload", "추가된 잠금·reload(with_lock, lock!, reload, FOR UPDATE)가 (1) select 별칭이나 메모리에만 있던 속성을 잃게 하는지 (2) 잠금 전에 읽은 객체를 잠금 뒤 그대로 저장하는지(stale read-modify-write) (3) 같은 저장소의 기존 잠금 관례와 다른지 확인하라.",
		regexp.MustCompile(`\bwith_lock\b|\.lock!|\.reload\b|FOR UPDATE|\.lock\(`), nil},
	{"state_transition", "바뀐 상태와 전이마다 그 상태로 들어가는 모든 경로(다른 전이, admin·cron·worker·API)를 나열하고 각 경로에서 새 조건이 지켜지는지, 옮기거나 바뀐 전이에 걸린 다른 콜백·부수효과(메일, 이벤트, 외부 호출)가 새 순서에서도 맞는지, 이미 끝난 상태로 같은 이벤트가 다시 와도 안전한지 확인하라.",
		regexp.MustCompile(`after_transition|before_transition|\btransition\b|\bevent\s+:|state_machine|aasm|_state!|_state\s*=|\.state\s*=`), regexp.MustCompile(`after_transition|before_transition|\btransition\b|state_machine|_state!`)},
	{"async_hop", "새로 생기거나 바뀐 비동기 작업마다, 같은 상태를 읽거나 쓰는 cron·worker·consumer와의 실행 순서를 표로 만들고, 순서가 바뀌거나 작업이 지연·실패·재시도·중복될 때도 결과가 맞는지, 대기 상태가 영원히 남는 경우(liveness)가 없는지 확인하라.",
		regexp.MustCompile(`perform_at|perform_in|perform_async|perform_later|set\(\s*wait|setTimeout|sendMessage|SendMessage|publish\(|enqueue`), nil},
	{"guard_removed", "없애거나 바꾼 가드·대기 조건이 간접적으로 보장하던 것(선행 상태, 데이터 준비, 중복 방지)이 무엇이었는지 찾고, 그 보장이 다른 곳에서 여전히 성립하는지 확인하라.",
		nil, regexp.MustCompile(`\breturn\b.*\b(if|unless)\b|\bnext\b.*\b(if|unless)\b|\bunless\b|\bwait|pending|ready\?|\bguard\b`)},
	{"exception_status", "새로 생기거나 바뀐 예외·상태 코드가 rescue_from, error renderer, middleware, 재시도 정책을 거쳐 실제로 어떤 HTTP 응답·로그·재시도로 이어지는지 끝까지 따라가 확인하라.",
		regexp.MustCompile(`\braise\b|Errors::|rescue_from|status_code|throw new|HttpException|status:\s*\d{3}`), nil},
	{"removed_contract", "지우거나 이름을 바꾼 응답 키·파라미터·메서드·export를 아직 쓰는 곳(이 저장소와 ./repos 아래 연관 저장소, ./facts.md의 검색 결과)이 있는지, 옛 클라이언트와 새 서버, 새 클라이언트와 옛 서버 조합이 모두 동작하는지 확인하라.",
		nil, regexp.MustCompile(`params\[|permit\(|\battribute\b|as_json|\bdef\s+\w+|export\s+(async\s+)?(function|const|class)|router\.|@(Get|Post|Put|Patch|Delete)\(|\w+:\s`)},
	{"input_parsing", "외부 입력을 파싱하는 코드의 경계값(빈 값, 음수, 소수, Infinity·NaN, 매우 큰 수, 잘못된 형식, 배열·객체가 아닌 값)과 그 예외가 rescue·catch 범위 안에 있는지 확인하라.",
		regexp.MustCompile(`Integer\(|Float\(|\.to_i\b|\.to_f\b|JSON\.parse|parseInt|parseFloat|Number\(|BigDecimal\(|\.to_sym\b`), nil},
}

const maxTriggerLocations = 30

func detectTriggers(fds []fileDiff) []Trigger {
	var out []Trigger
	for _, r := range triggerRules {
		var locs []string
		for _, fd := range fds {
			if testPath.MatchString(fd.Path) {
				continue
			}
			if r.added != nil {
				for _, l := range fd.Added {
					if r.added.MatchString(l.Text) && len(locs) < maxTriggerLocations {
						locs = append(locs, fmt.Sprintf("+ %s:%d: %s", fd.Path, l.N, strings.TrimSpace(l.Text)))
					}
				}
			}
			if r.removed != nil {
				for _, l := range fd.Removed {
					if r.removed.MatchString(l.Text) && len(locs) < maxTriggerLocations {
						locs = append(locs, fmt.Sprintf("- %s:(old %d): %s", fd.OldPath, l.N, strings.TrimSpace(l.Text)))
					}
				}
			}
		}
		if len(locs) > 0 {
			out = append(out, Trigger{Name: r.name, Question: r.question, Locations: locs})
		}
	}
	return out
}

var ciConfig = regexp.MustCompile(`(^|/)(azure-pipelines[^/]*\.ya?ml|\.github/workflows/[^/]+\.ya?ml|\.gitlab-ci\.yml|\.circleci/config\.yml|Rakefile|Makefile|package\.json|jest\.config\.[cm]?[jt]s|vitest\.config\.[cm]?[jt]s|\.rspec)$|(^|/)lib/tasks/[^/]+\.rake$|(^|/)azure-pipelines/.+\.ya?ml$`)

// newTestsTrigger lists new test files with the CI configuration files that mention one of their directories.
// Whether CI really runs them is left to a model check: a passing test is not the same as a test CI selects.
func newTestsTrigger(codeDir string, fds []fileDiff) (Trigger, bool) {
	var tests []string
	for _, fd := range fds {
		if fd.New && !fd.Deleted && testPath.MatchString(fd.Path) {
			tests = append(tests, fd.Path)
		}
	}
	if len(tests) == 0 {
		return Trigger{}, false
	}
	configs := map[string]string{}
	_ = filepath.WalkDir(codeDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(codeDir, p)
		if d.IsDir() {
			if skipDir(d.Name()) && rel != "." {
				return filepath.SkipDir
			}
			return nil
		}
		if ciConfig.MatchString(filepath.ToSlash(rel)) && len(configs) < 40 {
			if data, err := os.ReadFile(p); err == nil && len(data) < 1<<20 {
				configs[filepath.ToSlash(rel)] = string(data)
			}
		}
		return nil
	})
	var locs []string
	for _, t := range tests {
		var hits []string
		for dir := filepath.Dir(t); dir != "." && strings.Count(dir, "/") >= 1; dir = filepath.Dir(dir) {
			for name, text := range configs {
				if strings.Contains(text, dir) {
					hits = append(hits, name+" ("+dir+")")
				}
			}
			if len(hits) > 0 {
				break
			}
		}
		sort.Strings(hits)
		mention := "디렉터리를 언급하는 CI 설정 없음"
		if len(hits) > 0 {
			mention = "디렉터리를 언급하는 설정: " + strings.Join(hits, ", ")
		}
		locs = append(locs, fmt.Sprintf("+ %s (새 테스트) — %s", t, mention))
	}
	names := make([]string, 0, len(configs))
	for n := range configs {
		names = append(names, n)
	}
	sort.Strings(names)
	locs = append(locs, "CI 설정 후보 파일: "+strings.Join(names, ", "))
	return Trigger{Name: "new_tests_ci", Question: "새로 추가된 테스트 파일이 실제로 CI의 어느 작업·명령에서 선택되어 실행되는지 CI 설정과 그 설정이 부르는 rake/npm 스크립트를 따라가 확인하라. 테스트가 통과하는 것과 CI가 그 테스트를 실행하는 것은 별개다.", Locations: locs}, true
}

var suppression = regexp.MustCompile(`rubocop:(disable|todo)|T\.unsafe|T\.let\(.*T\.untyped|eslint-disable|@ts-ignore|@ts-expect-error|# type: ignore|//\s*nolint|# noqa`)

// suppressionCheck reports lint and type-check suppressions the change adds; they hide what the checks would catch.
func suppressionCheck(fds []fileDiff) (Check, []StoredFinding) {
	c := Check{Name: "suppression", Kind: "deterministic", Conclusion: "no_issue", Targets: []string{"change.diff의 추가된 줄"}}
	var out []StoredFinding
	for _, fd := range fds {
		for _, l := range fd.Added {
			if m := suppression.FindString(l.Text); m != "" {
				loc := fmt.Sprintf("%s:%d", fd.Path, l.N)
				c.Evidence = append(c.Evidence, loc+": "+strings.TrimSpace(l.Text))
				out = append(out, StoredFinding{Origin: "check", Pass: "check:suppression", Category: "structure", Finding: Finding{
					Title: "검사 억제 주석 추가: " + m, Severity: "low", Confidence: "high", File: fd.Path, Line: l.N,
					Condition: "이번 변경이 린트·타입 검사 억제를 추가함", CodePath: loc,
					Impact: "검사가 잡던 문제를 숨기고 팀 규칙에 어긋날 수 있음", Evidence: strings.TrimSpace(l.Text), Scope: "in"}})
			}
		}
	}
	if len(out) > 0 {
		c.Conclusion = "issue"
	}
	return c, out
}

var definition = regexp.MustCompile(`^\s*(?:def\s+(?:self\.)?([A-Za-z_]\w*[?!]?)|(?:export\s+)?(?:async\s+)?function\s+([A-Za-z_$][\w$]*)|export\s+(?:const|let|class)\s+([A-Za-z_$][\w$]*)|func\s+(?:\([^)]*\)\s*)?([A-Za-z_]\w*))`)

func definedNames(lines []diffLine) map[string]bool {
	out := map[string]bool{}
	for _, l := range lines {
		if m := definition.FindStringSubmatch(l.Text); m != nil {
			for _, g := range m[1:] {
				if len(g) >= 4 {
					out[g] = true
				}
			}
		}
	}
	return out
}

// removedDefinitionCheck finds definitions the change removes but the code (or a sibling repo) still references.
func removedDefinitionCheck(work string, fds []fileDiff) (Check, []StoredFinding) {
	c := Check{Name: "removed_definition_refs", Kind: "deterministic", Conclusion: "no_issue"}
	removed, added := map[string]bool{}, map[string]bool{}
	for _, fd := range fds {
		for n := range definedNames(fd.Removed) {
			removed[n] = true
		}
		for n := range definedNames(fd.Added) {
			added[n] = true
		}
	}
	var names []string
	for n := range removed {
		if !added[n] {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		c.Conclusion, c.NotChecked = "no_issue", "지운 정의 없음"
		return c, nil
	}
	c.Targets = names
	refs := grepWords(work, []string{"repo", "repos"}, names, 5)
	var out []StoredFinding
	for _, n := range names {
		hits := refs[n]
		if len(hits) == 0 {
			continue
		}
		c.Evidence = append(c.Evidence, n+": "+strings.Join(hits, "; "))
		file, line := splitLoc(hits[0])
		out = append(out, StoredFinding{Origin: "check", Pass: "check:removed_definition_refs", Category: "defect", Finding: Finding{
			Title: "지운 정의 " + n + "를 아직 참조함", Severity: "medium", Confidence: "medium", File: file, Line: line,
			Condition: "이번 변경이 " + n + " 정의를 지웠지만 다른 코드가 여전히 같은 이름을 씀", CodePath: strings.Join(hits, "; "),
			Impact:   "호출 시 NoMethodError·undefined 오류가 나거나 다른 정의로 바뀌어 동작할 수 있음(같은 이름의 다른 정의일 수 있으니 확인 필요)",
			Evidence: strings.Join(hits, "\n"), Scope: "in"}})
	}
	if len(out) > 0 {
		c.Conclusion = "issue"
	}
	return c, out
}

func splitLoc(hit string) (string, int) {
	parts := strings.SplitN(hit, ":", 3)
	if len(parts) < 2 {
		return hit, 0
	}
	n, _ := strconv.Atoi(parts[1])
	return parts[0], n
}

// mergeCheck reports conflicts between the reviewed head and other open changes (e.g. other PR heads).
func mergeCheck(ctx context.Context, repo, head string, revs []string) (Check, []StoredFinding) {
	c := Check{Name: "merge_conflicts", Kind: "deterministic", Conclusion: "no_issue", Targets: revs}
	if len(revs) == 0 {
		c.Conclusion, c.NotChecked = "not_run", "--merge-with 없음"
		return c, nil
	}
	if head == "" {
		c.Conclusion, c.NotChecked = "not_run", "리뷰 대상에 commit이 없음(patch·working tree)"
		return c, nil
	}
	var out []StoredFinding
	for _, rev := range revs {
		cmd := exec.CommandContext(ctx, "git", "-C", repo, "merge-tree", "--write-tree", "--name-only", "--no-messages", head, rev)
		data, err := cmd.Output()
		var exit *exec.ExitError
		if err != nil && !(errors.As(err, &exit) && exit.ExitCode() == 1) {
			c.Evidence = append(c.Evidence, rev+": merge-tree 실패: "+err.Error())
			c.Conclusion = "cannot_determine"
			continue
		}
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		if err == nil || len(lines) < 2 {
			c.Evidence = append(c.Evidence, rev+": 충돌 없음")
			continue
		}
		files := lines[1:]
		c.Evidence = append(c.Evidence, rev+": 충돌 "+strings.Join(files, ", "))
		out = append(out, StoredFinding{Origin: "check", Pass: "check:merge_conflicts", Category: "defect", Finding: Finding{
			Title: "열린 변경 " + rev + "와 충돌", Severity: "medium", Confidence: "high", File: files[0], Line: 0,
			Condition: "이 변경과 " + rev + "를 함께 병합하면 충돌함", CodePath: strings.Join(files, ", "),
			Impact: "먼저 병합되는 쪽에 따라 다른 쪽이 깨지거나 수동 해결이 필요함", Evidence: "git merge-tree --write-tree " + head + " " + rev, Scope: "in"}})
	}
	if len(out) > 0 {
		c.Conclusion = "issue"
	}
	return c, out
}

var codeExt = map[string]bool{".rb": true, ".rake": true, ".erb": true, ".ts": true, ".tsx": true, ".js": true, ".jsx": true, ".mjs": true,
	".py": true, ".go": true, ".java": true, ".kt": true, ".sql": true, ".yml": true, ".yaml": true, ".json": true, ".rbi": true}

func skipDir(name string) bool {
	switch name {
	case ".git", "node_modules", "vendor", "dist", "build", "tmp", "log", "coverage", "public", ".next":
		return true
	}
	return false
}

// grepWords finds whole-word references to names under the given workspace subdirectories (paths are workspace-relative).
func grepWords(work string, roots, names []string, perName int) map[string][]string {
	out := map[string][]string{}
	if len(names) == 0 {
		return out
	}
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = regexp.QuoteMeta(n)
	}
	re := regexp.MustCompile(`(?:^|[^\w$])(` + strings.Join(quoted, "|") + `)(?:[^\w$?!]|$)`)
	for _, root := range roots {
		base := filepath.Join(work, root)
		_ = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if p != base && skipDir(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if !codeExt[filepath.Ext(p)] {
				return nil
			}
			data, err := os.ReadFile(p)
			if err != nil || len(data) > 2<<20 || !re.Match(data) {
				return nil
			}
			rel, _ := filepath.Rel(work, p)
			for i, line := range strings.Split(string(data), "\n") {
				for _, m := range re.FindAllStringSubmatch(line, -1) {
					if len(out[m[1]]) < perName && !definition.MatchString(line) {
						out[m[1]] = append(out[m[1]], fmt.Sprintf("%s:%d: %s", filepath.ToSlash(rel), i+1, truncate(strings.TrimSpace(line), 160)))
					}
				}
			}
			return nil
		})
	}
	return out
}

var contractKey = regexp.MustCompile(`params\[:(\w{3,})\]|:(\w{3,})\s*=>|["'](\w{3,})["']\s*:|\b(\w{3,}):\s|["'](/(?:api|v\d)[\w/{}:-]+)["']`)

// consumerSearch looks up identifiers removed from contract-like lines in the sibling repositories.
func consumerSearch(work string, fds []fileDiff) []string {
	if _, err := os.Stat(filepath.Join(work, "repos")); err != nil {
		return nil
	}
	set := map[string]bool{}
	for _, fd := range fds {
		if testPath.MatchString(fd.Path) {
			continue
		}
		for n := range definedNames(fd.Removed) {
			set[n] = true
		}
		for _, l := range fd.Removed {
			for _, m := range contractKey.FindAllStringSubmatch(l.Text, -1) {
				for _, g := range m[1:] {
					if g != "" {
						set[g] = true
					}
				}
			}
		}
	}
	var names []string
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) > 60 {
		names = names[:60]
	}
	refs := grepWords(work, []string{"repos"}, names, 3)
	var out []string
	for _, n := range names {
		for _, h := range refs[n] {
			out = append(out, n+" → "+h)
		}
	}
	return out
}

var layerOf = []struct {
	re    *regexp.Regexp
	layer string
}{
	{regexp.MustCompile(`/controllers?/`), "controller"}, {regexp.MustCompile(`/services?/`), "service"},
	{regexp.MustCompile(`/repositor(y|ies)/`), "repository"}, {regexp.MustCompile(`/concerns/`), "concern"},
	{regexp.MustCompile(`/models?/`), "model"}, {regexp.MustCompile(`/workers?/|/jobs?/`), "worker/job"},
	{regexp.MustCompile(`/serializers?/`), "serializer"}, {regexp.MustCompile(`(^|/)lib/`), "lib"},
	{regexp.MustCompile(`/migrate/`), "migration"}, {regexp.MustCompile(`/domain/`), "domain"}, {regexp.MustCompile(`/api/`), "api"},
}

// structureFacts describes new files, their neighbors and same-named definitions elsewhere for the structure pass.
func structureFacts(work string, fds []fileDiff) []string {
	codeDir := filepath.Join(work, "repo")
	var lines []string
	var newNames []string
	for _, fd := range fds {
		if testPath.MatchString(fd.Path) {
			continue
		}
		for n := range definedNames(fd.Added) {
			newNames = append(newNames, n)
		}
		if !fd.New {
			continue
		}
		layer := "기타"
		for _, l := range layerOf {
			if l.re.MatchString("/" + fd.Path) {
				layer = l.layer
				break
			}
		}
		var neighbors []string
		if entries, err := os.ReadDir(filepath.Join(codeDir, filepath.Dir(fd.Path))); err == nil {
			for _, e := range entries {
				if !e.IsDir() && e.Name() != filepath.Base(fd.Path) && len(neighbors) < 5 {
					neighbors = append(neighbors, e.Name())
				}
			}
		}
		lines = append(lines, fmt.Sprintf("새 파일 %s (계층: %s) 이웃: %s", fd.Path, layer, strings.Join(neighbors, ", ")))
	}
	sort.Strings(newNames)
	defs := map[string][]string{}
	if len(newNames) > 0 {
		defRe := regexp.MustCompile(`(?:def\s+(?:self\.)?|function\s+|const\s+|class\s+|func\s+)(` + quoteAll(newNames) + `)\b`)
		_ = filepath.WalkDir(codeDir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if p != codeDir && skipDir(d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if !codeExt[filepath.Ext(p)] {
				return nil
			}
			data, err := os.ReadFile(p)
			if err != nil || len(data) > 2<<20 || !defRe.Match(data) {
				return nil
			}
			rel, _ := filepath.Rel(codeDir, p)
			for i, line := range strings.Split(string(data), "\n") {
				if m := defRe.FindStringSubmatch(line); m != nil && len(defs[m[1]]) < 4 {
					defs[m[1]] = append(defs[m[1]], fmt.Sprintf("%s:%d", filepath.ToSlash(rel), i+1))
				}
			}
			return nil
		})
	}
	for _, n := range newNames {
		if len(defs[n]) > 1 {
			lines = append(lines, fmt.Sprintf("같은 이름의 정의 %s: %s", n, strings.Join(defs[n], ", ")))
		}
	}
	return lines
}

func quoteAll(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = regexp.QuoteMeta(n)
	}
	return strings.Join(q, "|")
}

// blobHash is the git object id of content, so reviewed files can be matched with commits later.
func blobHash(data []byte) string {
	h := sha1.New()
	fmt.Fprintf(h, "blob %d\x00", len(data))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func reviewedFiles(codeDir string, fds []fileDiff) map[string]string {
	out := map[string]string{}
	for _, fd := range fds {
		if fd.Deleted {
			out[fd.OldPath] = "deleted"
			continue
		}
		if data, err := os.ReadFile(filepath.Join(codeDir, fd.Path)); err == nil {
			out[fd.Path] = blobHash(data)
		}
	}
	return out
}

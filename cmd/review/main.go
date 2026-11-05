// review is a non-interactive code review CLI for AI agents. Every command prints one JSON document
// to stdout. Exit codes: 0 success, 1 runtime failure (including a recorded run that failed), 2 usage error.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/lnyarl/review-cli/internal/review"
)

const usage = `review — code review CLI for AI agents. All output is JSON on stdout.
Exit codes: 0 success, 1 failure (a failed review is still printed and recorded), 2 usage error.
` + "`review <command> --help`" + ` prints this text. On a terminal help is plain text, otherwise {"usage": ...}.

Commands:
  review run   --repo PATH TARGET [--issue TEXT | --issue-file PATH] [--effort auto|low|high] [--keep-workspace]
               [--context-file PATH]... [--with-repo NAME=PATH[@REV]]... [--merge-with REV]...
               [--passes defect,triggers,structure] [--targeted]
               TARGET is one of
                 --base REV --patch FILE|-     any scope as a patch against REV (- reads stdin); add
                                               --label TEXT to record the scope, e.g. staged
                 --base REV --head REV         commit range base...head
                 --pr ID --collector-db PATH   Azure DevOps PR from the collector DB
                 --working-tree [--paths a,b]  uncommitted tracked and untracked changes against HEAD
               Review a change in passes: deterministic checks, the broad defect pass (auto routes effort by
               diff size/risk and re-reviews a low result with a high-severity finding at high), one narrow
               check per trigger found in the diff (locks, state transitions, async jobs, removed guards,
               exceptions, removed contracts, input parsing, new tests vs CI) and a structure pass.
               A trigger check without a conclusion and evidence makes the run incomplete (exit 1).
               --context-file adds specs, rules, CI results (file or directory, repeatable).
               --with-repo exports a related repository read-only under ./repos/NAME (repeatable).
               --merge-with checks conflicts with another open change (repeatable; needs a commit target).
               --targeted marks a run that checks specific claims; it does not count as coverage and runs
               only the defect pass unless --passes is given.
               Staged changes: git diff --cached --binary | review run --repo . --base HEAD --patch - --label staged
  review coverage --repo PATH --base REV --head REV
               Show which files of base...head were reviewed in exactly their head content. Files changed
               after their last review are uncovered; all_covered=false means run the suggested review.
  review show  RUN_ID                     Print a run with findings and their latest verdicts.
  review list  [--limit N]                Print recent runs.
  review add-finding RUN_ID --file PATH --line N --title TEXT [--severity high|medium|low (default medium)]
               [--confidence high|medium|low (default high)] [--condition TEXT] [--code-path TEXT] [--impact TEXT]
               [--evidence TEXT] [--origin human|agent (default human)] [--verdict defect|useful (default defect)]
               [--note TEXT] [--actor NAME]
               Add a finding the reviewer missed. origin human (a person: "you missed this") also records the
               verdict, defect or useful (not a defect but a valuable review comment); origin agent (e.g. the
               ai-review skill's own finding) takes no verdict and waits for a person's judgment.
  review judge FINDING_ID VERDICT [--note TEXT] [--actor NAME]
               VERDICT: defect|useful|false_positive|unsure|duplicate (결함|유용|오탐|미확정|중복). Append-only.
               useful = not a defect but a valuable review comment.
  review eval  --suite PATH [--effort auto|low|high] [--parallel N (default 4)] [--passes ...]
               Re-review a fixed suite, judge per expected comment whether a finding has the same cause
               (matched/partial), and compare with the previous eval of that suite.

Global flags (before or after the command flags):
  --db PATH       ledger SQLite (default <user config dir>/review-cli/review.sqlite, env REVIEW_DB)
  --model ID      reviewer model (default claude-sonnet-5-5, env REVIEW_MODEL)
  --claude PATH   Claude Code executable (default claude)

Workspaces are created under the user cache dir (env REVIEW_WORK_ROOT) and removed after each run.
`

type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

// helpText is printed as plain text on a terminal and as {"usage": ...} otherwise.
type helpText struct{}

// failedResult prints its output as the result document but exits 1, e.g. a recorded run that failed.
type failedResult struct{ out any }

func (e failedResult) Error() string { return "failed" }

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	info, err := os.Stdout.Stat()
	tty := err == nil && info.Mode()&os.ModeCharDevice != 0
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, tty))
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, tty bool) int {
	out, err := dispatch(ctx, args, stdin)
	if _, ok := out.(helpText); ok && err == nil {
		if tty {
			_, _ = io.WriteString(stdout, usage)
			return 0
		}
		out = map[string]string{"usage": usage}
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", " ")
	enc.SetEscapeHTML(false)
	var fr failedResult
	if errors.As(err, &fr) {
		_ = enc.Encode(fr.out)
		return 1
	}
	if err != nil {
		code, kind := 1, "runtime"
		var ue usageError
		if errors.As(err, &ue) {
			code, kind = 2, "usage"
		}
		_ = enc.Encode(map[string]any{"error": map[string]string{"kind": kind, "message": err.Error()}})
		return code
	}
	_ = enc.Encode(out)
	return 0
}

type globals struct {
	db, model, claude string
}

func addGlobals(fs *flag.FlagSet, g *globals) {
	fs.StringVar(&g.db, "db", envOr("REVIEW_DB", defaultDB()), "ledger path")
	fs.StringVar(&g.model, "model", envOr("REVIEW_MODEL", "claude-sonnet-5-5"), "reviewer model")
	fs.StringVar(&g.claude, "claude", "claude", "Claude Code executable")
}

// defaultDB is a fixed per-user location so runs from any directory or worktree share one ledger.
func defaultDB() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return filepath.Join("data", "review", "review.sqlite")
	}
	return filepath.Join(dir, "review-cli", "review.sqlite")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func dispatch(ctx context.Context, args []string, stdin io.Reader) (any, error) {
	if len(args) == 0 || args[0] == "help" {
		return helpText{}, nil
	}
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "-help" {
			return helpText{}, nil
		}
	}
	cmd, rest := args[0], args[1:]
	var g globals
	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	addGlobals(fs, &g)
	switch cmd {
	case "run":
		return cmdRun(ctx, fs, &g, rest, stdin)
	case "show":
		return withStore(fs, &g, rest, 1, func(s *review.Store, pos []string) (any, error) { return s.GetRun(ctx, pos[0]) })
	case "list":
		limit := fs.Int("limit", 20, "")
		return withStore(fs, &g, rest, 0, func(s *review.Store, _ []string) (any, error) { return s.ListRuns(ctx, *limit) })
	case "judge":
		note := fs.String("note", "", "")
		actor := fs.String("actor", envOr("USER", "unknown"), "")
		return withStore(fs, &g, rest, 2, func(s *review.Store, pos []string) (any, error) {
			if _, ok := review.NormalizeVerdict(pos[1]); !ok {
				return nil, usageError{"VERDICT must be " + review.VerdictHelp}
			}
			v, err := s.Judge(ctx, pos[0], pos[1], *note, *actor)
			return map[string]any{"finding_id": pos[0], "verdict": v}, err
		})
	case "eval":
		return cmdEval(ctx, fs, &g, rest)
	case "coverage":
		repo := fs.String("repo", "", "")
		base := fs.String("base", "", "")
		head := fs.String("head", "", "")
		return withStore(fs, &g, rest, 0, func(s *review.Store, _ []string) (any, error) {
			if *repo == "" || *base == "" || *head == "" {
				return nil, usageError{"coverage needs --repo, --base and --head"}
			}
			return review.ComputeCoverage(ctx, s, *repo, *base, *head)
		})
	case "add-finding":
		var f review.Finding
		fs.StringVar(&f.File, "file", "", "")
		fs.IntVar(&f.Line, "line", -1, "")
		fs.StringVar(&f.Title, "title", "", "")
		fs.StringVar(&f.Severity, "severity", "medium", "")
		fs.StringVar(&f.Confidence, "confidence", "high", "")
		fs.StringVar(&f.Condition, "condition", "", "")
		fs.StringVar(&f.CodePath, "code-path", "", "")
		fs.StringVar(&f.Impact, "impact", "", "")
		fs.StringVar(&f.Evidence, "evidence", "", "")
		origin := fs.String("origin", "human", "")
		verdict := fs.String("verdict", "", "")
		note := fs.String("note", "", "")
		actor := fs.String("actor", envOr("USER", "unknown"), "")
		return withStore(fs, &g, rest, 1, func(s *review.Store, pos []string) (any, error) {
			if f.File == "" || f.Title == "" || f.Line < 0 {
				return nil, usageError{"add-finding needs --file, --line and --title"}
			}
			if *origin != "human" && *origin != "agent" {
				return nil, usageError{"--origin must be human or agent"}
			}
			if v, ok := review.NormalizeVerdict(*verdict); *verdict != "" && (!ok || *origin != "human" || (v != "defect" && v != "useful")) {
				return nil, usageError{"--verdict is defect or useful (결함|유용) and only for --origin human"}
			}
			return s.AddFinding(ctx, pos[0], *origin, f, *verdict, *actor, *note)
		})
	}
	return nil, usageError{fmt.Sprintf("unknown command %q; run `review help`", cmd)}
}

// parse accepts flags and positional arguments in any order.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, usageError{err.Error()}
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func withStore(fs *flag.FlagSet, g *globals, args []string, npos int, fn func(*review.Store, []string) (any, error)) (any, error) {
	pos, err := parse(fs, args)
	if err != nil {
		return nil, err
	}
	if len(pos) != npos {
		return nil, usageError{fmt.Sprintf("%s takes %d positional argument(s); run `review help`", fs.Name(), npos)}
	}
	s, err := review.OpenStore(g.db)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	return fn(s, pos)
}

func service(g *globals, keep bool) (*review.Service, error) {
	s, err := review.OpenStore(g.db)
	if err != nil {
		return nil, err
	}
	work, err := workRoot()
	if err != nil {
		s.Close()
		return nil, err
	}
	return &review.Service{Store: s, Runner: review.ClaudeRunner{Executable: g.claude, ModelID: g.model, Timeout: 20 * time.Minute},
		WorkRoot: work, KeepWorkspace: keep}, nil
}

// workRoot keeps workspaces on a short ASCII path: the reviewer types absolute paths and misspelled a
// non-ASCII repository path, which the sandbox then denied.
func workRoot() (string, error) {
	if v := os.Getenv("REVIEW_WORK_ROOT"); v != "" {
		return v, nil
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cache, "review-cli", "work"), nil
}

func validEffort(e string) error {
	switch e {
	case "auto", "low", "high":
		return nil
	}
	return usageError{"--effort must be auto, low or high"}
}

func cmdRun(ctx context.Context, fs *flag.FlagSet, g *globals, args []string, stdin io.Reader) (any, error) {
	repo := fs.String("repo", "", "")
	base := fs.String("base", "", "")
	head := fs.String("head", "", "")
	patch := fs.String("patch", "", "")
	label := fs.String("label", "", "")
	pr := fs.Int("pr", 0, "")
	collector := fs.String("collector-db", "", "")
	wt := fs.Bool("working-tree", false, "")
	paths := fs.String("paths", "", "")
	issue := fs.String("issue", "", "")
	issueFile := fs.String("issue-file", "", "")
	effort := fs.String("effort", "auto", "")
	keep := fs.Bool("keep-workspace", false, "")
	targeted := fs.Bool("targeted", false, "")
	var contexts, withRepos, mergeWith stringList
	fs.Var(&contexts, "context-file", "")
	fs.Var(&withRepos, "with-repo", "")
	fs.Var(&mergeWith, "merge-with", "")
	passes := fs.String("passes", "", "")
	pos, err := parse(fs, args)
	if err != nil {
		return nil, err
	}
	if len(pos) != 0 || *repo == "" {
		return nil, usageError{"run needs --repo and no positional arguments; run `review help`"}
	}
	opts, err := runOptions(*effort, *passes, *targeted, contexts, withRepos, mergeWith)
	if err != nil {
		return nil, err
	}
	if *issueFile != "" {
		if *issue != "" {
			return nil, usageError{"use --issue or --issue-file, not both"}
		}
		data, err := os.ReadFile(*issueFile)
		if err != nil {
			return nil, err
		}
		*issue = string(data)
	}
	var t review.Target
	switch modes := btoi(*base != "" || *head != "" || *patch != "") + btoi(*pr != 0) + btoi(*wt); {
	case modes != 1:
		return nil, usageError{"choose exactly one target: --base/--patch, --base/--head, --pr or --working-tree"}
	case *patch != "":
		if *base == "" || *head != "" {
			return nil, usageError{"--patch needs --base and no --head"}
		}
		var data []byte
		if *patch == "-" {
			data, err = io.ReadAll(stdin)
		} else {
			data, err = os.ReadFile(*patch)
		}
		if err != nil {
			return nil, err
		}
		t = &review.Patch{Repo: *repo, Base: *base, Label: *label, Diff: data, Issue: *issue}
	case *base != "" || *head != "":
		if *base == "" || *head == "" {
			return nil, usageError{"--base and --head are both required"}
		}
		t = &review.CommitRange{Repo: *repo, Base: *base, Head: *head, Issue: *issue}
	case *pr != 0:
		if *collector == "" {
			return nil, usageError{"--pr needs --collector-db"}
		}
		t = &review.AdoPR{CollectorDB: *collector, Repo: *repo, PRID: *pr, Issue: *issue}
	default:
		var list []string
		for _, p := range strings.Split(*paths, ",") {
			if p = strings.TrimSpace(p); p != "" {
				list = append(list, p)
			}
		}
		t = &review.WorkingTree{Repo: *repo, Paths: list, Issue: *issue}
	}
	svc, err := service(g, *keep)
	if err != nil {
		return nil, err
	}
	defer svc.Store.Close()
	rep, err := svc.Run(ctx, t, opts)
	if err == nil && (rep.Run.Status != "succeeded" || !rep.Run.Complete) {
		return nil, failedResult{rep}
	}
	return rep, err
}

// stringList is a repeatable flag.
type stringList []string

func (l *stringList) String() string     { return strings.Join(*l, ",") }
func (l *stringList) Set(v string) error { *l = append(*l, v); return nil }

func runOptions(effort, passes string, targeted bool, contexts, withRepos, mergeWith []string) (review.Options, error) {
	o := review.Options{Effort: effort, Context: contexts, MergeWith: mergeWith}
	if err := validEffort(effort); err != nil {
		return o, err
	}
	for _, r := range withRepos {
		ref, err := review.ParseRepoRef(r)
		if err != nil {
			return o, usageError{err.Error()}
		}
		o.Repos = append(o.Repos, ref)
	}
	if passes != "" {
		for _, p := range strings.Split(passes, ",") {
			p = strings.TrimSpace(p)
			if p != review.PassDefect && p != review.PassTriggers && p != review.PassStructure {
				return o, usageError{"--passes takes defect, triggers and structure"}
			}
			o.Passes = append(o.Passes, p)
		}
	}
	if targeted {
		o.Purpose = "targeted"
		if len(o.Passes) == 0 {
			o.Passes = []string{review.PassDefect}
		}
	}
	return o, nil
}

func cmdEval(ctx context.Context, fs *flag.FlagSet, g *globals, args []string) (any, error) {
	suitePath := fs.String("suite", "", "")
	effort := fs.String("effort", "auto", "")
	parallel := fs.Int("parallel", 4, "")
	passes := fs.String("passes", "", "")
	pos, err := parse(fs, args)
	if err != nil {
		return nil, err
	}
	if len(pos) != 0 || *suitePath == "" || *parallel < 1 {
		return nil, usageError{"eval needs --suite; run `review help`"}
	}
	opts, err := runOptions(*effort, *passes, false, nil, nil, nil)
	if err != nil {
		return nil, err
	}
	suite, dir, err := review.LoadSuite(*suitePath)
	if err != nil {
		return nil, err
	}
	svc, err := service(g, false)
	if err != nil {
		return nil, err
	}
	defer svc.Store.Close()
	res, prev, err := svc.Eval(ctx, suite, dir, opts, *parallel)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"result": res, "previous": prev}
	if res.Failed > 0 {
		return nil, failedResult{out}
	}
	return out, nil
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

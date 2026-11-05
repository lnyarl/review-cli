package review

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

// Store is the review ledger, a SQLite file of its own.
// Verdicts are append-only; the current verdict of a finding is its latest row.
type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS runs(
 id TEXT PRIMARY KEY, created_at TEXT NOT NULL, target_kind TEXT NOT NULL, target_json TEXT NOT NULL,
 issue TEXT NOT NULL, diff TEXT NOT NULL, effort TEXT NOT NULL, route_json TEXT NOT NULL, escalated_from TEXT REFERENCES runs(id),
 model TEXT NOT NULL, observed_model TEXT NOT NULL, status TEXT NOT NULL CHECK(status IN ('succeeded','failed')), error TEXT NOT NULL,
 seconds REAL NOT NULL, tool_uses INTEGER NOT NULL, inspected_json TEXT NOT NULL, not_checked TEXT NOT NULL,
 warnings_json TEXT NOT NULL DEFAULT '[]', purpose TEXT NOT NULL DEFAULT 'review', reviewed_json TEXT NOT NULL DEFAULT '{}',
 checks_json TEXT NOT NULL DEFAULT '[]', complete INTEGER NOT NULL DEFAULT 1);
CREATE TABLE IF NOT EXISTS findings(
 id TEXT PRIMARY KEY, run_id TEXT NOT NULL REFERENCES runs(id), ordinal INTEGER NOT NULL, title TEXT NOT NULL,
 severity TEXT NOT NULL, confidence TEXT NOT NULL, file TEXT NOT NULL, line INTEGER NOT NULL, condition TEXT NOT NULL,
 code_path TEXT NOT NULL, impact TEXT NOT NULL, evidence TEXT NOT NULL,
 origin TEXT NOT NULL DEFAULT 'model', pass TEXT NOT NULL DEFAULT 'defect', category TEXT NOT NULL DEFAULT 'defect',
 scope TEXT NOT NULL DEFAULT 'in', UNIQUE(run_id, ordinal));
CREATE TABLE IF NOT EXISTS verdicts(
 id INTEGER PRIMARY KEY AUTOINCREMENT, finding_id TEXT NOT NULL REFERENCES findings(id),
 verdict TEXT NOT NULL CHECK(verdict IN ('defect','useful','false_positive','unsure','duplicate')),
 note TEXT NOT NULL, actor TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS evals(
 id TEXT PRIMARY KEY, created_at TEXT NOT NULL, suite TEXT NOT NULL, options_json TEXT NOT NULL, result_json TEXT NOT NULL);
`

func OpenStore(path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: abs}
	q := u.Query()
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "foreign_keys(1)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	for _, c := range [][3]string{
		{"runs", "warnings_json", "TEXT NOT NULL DEFAULT '[]'"},
		{"runs", "purpose", "TEXT NOT NULL DEFAULT 'review'"},
		{"runs", "reviewed_json", "TEXT NOT NULL DEFAULT '{}'"},
		{"runs", "checks_json", "TEXT NOT NULL DEFAULT '[]'"},
		{"runs", "complete", "INTEGER NOT NULL DEFAULT 1"},
		{"findings", "origin", "TEXT NOT NULL DEFAULT 'model'"},
		{"findings", "pass", "TEXT NOT NULL DEFAULT 'defect'"},
		{"findings", "category", "TEXT NOT NULL DEFAULT 'defect'"},
		{"findings", "scope", "TEXT NOT NULL DEFAULT 'in'"},
	} {
		if err := addColumn(db, c[0], c[1], c[2]); err != nil {
			db.Close()
			return nil, err
		}
	}
	if err := widenVerdicts(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// widenVerdicts rebuilds a verdicts table created before 'useful' existed; SQLite cannot alter a CHECK.
func widenVerdicts(db *sql.DB) error {
	var ddl string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='verdicts'`).Scan(&ddl); err != nil || strings.Contains(ddl, "'useful'") {
		return err
	}
	// SQLite's table rebuild procedure: foreign keys off outside the transaction, on again afterwards.
	if _, err := db.Exec(`PRAGMA foreign_keys=OFF`); err != nil {
		return err
	}
	defer db.Exec(`PRAGMA foreign_keys=ON`)
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`CREATE TABLE verdicts_new(id INTEGER PRIMARY KEY AUTOINCREMENT, finding_id TEXT NOT NULL REFERENCES findings(id),
		 verdict TEXT NOT NULL CHECK(verdict IN ('defect','useful','false_positive','unsure','duplicate')),
		 note TEXT NOT NULL, actor TEXT NOT NULL, created_at TEXT NOT NULL)`,
		`INSERT INTO verdicts_new(id,finding_id,verdict,note,actor,created_at) SELECT id,finding_id,verdict,note,actor,created_at FROM verdicts`,
		`DROP TABLE verdicts`,
		`ALTER TABLE verdicts_new RENAME TO verdicts`,
	} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// addColumn upgrades a ledger created before the column existed.
func addColumn(db *sql.DB, table, column, def string) error {
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name=?`, table, column).Scan(&n); err != nil || n > 0 {
		return err
	}
	_, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, def))
	return err
}

func (s *Store) Close() error { return s.db.Close() }

type Run struct {
	ID            string            `json:"id"`
	CreatedAt     string            `json:"created_at"`
	TargetKind    string            `json:"target_kind"`
	Target        map[string]string `json:"target"`
	Issue         string            `json:"-"`
	Diff          string            `json:"-"`
	Effort        string            `json:"effort"`
	Route         Route             `json:"route"`
	EscalatedFrom string            `json:"escalated_from,omitempty"`
	Model         string            `json:"model"`
	ObservedModel string            `json:"observed_model"`
	Status        string            `json:"status"`
	Error         string            `json:"error,omitempty"`
	Seconds       float64           `json:"seconds"`
	ToolUses      int               `json:"tool_uses"`
	Inspected     []string          `json:"inspected_files"`
	NotChecked    string            `json:"not_checked"`
	Warnings      []string          `json:"warnings"`
	// Purpose "targeted" marks a run that checked specific claims; it does not count as review coverage.
	Purpose string `json:"purpose"`
	// Reviewed maps each changed path to the git blob hash of the reviewed content ("deleted" when removed).
	Reviewed map[string]string `json:"reviewed_files"`
	Checks   []Check           `json:"checks"`
	// Complete is false when a required check produced no conclusion with evidence.
	Complete bool            `json:"complete"`
	Findings []StoredFinding `json:"findings"`
}

// Check is one required check of a run: a trigger question answered by the model or a deterministic check.
type Check struct {
	Name       string   `json:"name"`
	Kind       string   `json:"kind"` // trigger | deterministic
	Locations  []string `json:"locations,omitempty"`
	Conclusion string   `json:"conclusion"` // issue | no_issue | cannot_determine | not_run
	Targets    []string `json:"targets_checked,omitempty"`
	Evidence   []string `json:"evidence,omitempty"`
	NotChecked string   `json:"not_checked,omitempty"`
	Error      string   `json:"error,omitempty"`
}

// Origin says who reported a finding: the reviewer model of the run (model), a deterministic check (check),
// a person adding something the model missed (human), or an agent such as the ai-review skill (agent).
// Pass is the pass that produced it (defect, trigger:<name>, structure, check:<name>).
type StoredFinding struct {
	ID       string `json:"id"`
	Origin   string `json:"origin"`
	Pass     string `json:"pass"`
	Category string `json:"category"`
	Finding
	Verdict *Verdict `json:"verdict,omitempty"`
}

type Verdict struct {
	Verdict   string `json:"verdict"`
	Note      string `json:"note,omitempty"`
	Actor     string `json:"actor"`
	CreatedAt string `json:"created_at"`
}

func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// SaveRun stores a run and its findings in one transaction and assigns IDs.
func (s *Store) SaveRun(ctx context.Context, r *Run) error {
	r.ID, r.CreatedAt = uuid.NewString(), now()
	target, _ := json.Marshal(r.Target)
	route, _ := json.Marshal(r.Route)
	inspected, _ := json.Marshal(r.Inspected)
	if r.Warnings == nil {
		r.Warnings = []string{}
	}
	warnings, _ := json.Marshal(r.Warnings)
	if r.Purpose == "" {
		r.Purpose = "review"
	}
	if r.Reviewed == nil {
		r.Reviewed = map[string]string{}
	}
	if r.Checks == nil {
		r.Checks = []Check{}
	}
	reviewed, _ := json.Marshal(r.Reviewed)
	checks, _ := json.Marshal(r.Checks)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var escalated any
	if r.EscalatedFrom != "" {
		escalated = r.EscalatedFrom
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO runs(id,created_at,target_kind,target_json,issue,diff,effort,route_json,escalated_from,
		model,observed_model,status,error,seconds,tool_uses,inspected_json,not_checked,warnings_json,purpose,reviewed_json,checks_json,complete)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.CreatedAt, r.TargetKind, string(target), r.Issue, r.Diff, r.Effort, string(route), escalated, r.Model, r.ObservedModel,
		r.Status, r.Error, r.Seconds, r.ToolUses, string(inspected), r.NotChecked, string(warnings), r.Purpose, string(reviewed),
		string(checks), r.Complete); err != nil {
		return err
	}
	for i := range r.Findings {
		f := &r.Findings[i]
		f.ID = fmt.Sprintf("%s-%d", r.ID[:8], i+1)
		if f.Origin == "" {
			f.Origin = "model"
		}
		if err := insertFinding(ctx, tx, r.ID, i+1, f); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func insertFinding(ctx context.Context, tx *sql.Tx, runID string, ordinal int, f *StoredFinding) error {
	if f.Pass == "" {
		f.Pass = "defect"
	}
	if f.Category == "" {
		f.Category = "defect"
	}
	if f.Scope == "" {
		f.Scope = "in"
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO findings(id,run_id,ordinal,title,severity,confidence,file,line,condition,code_path,impact,evidence,
		origin,pass,category,scope) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, f.ID, runID, ordinal, f.Title, f.Severity, f.Confidence, f.File,
		f.Line, f.Condition, f.CodePath, f.Impact, f.Evidence, f.Origin, f.Pass, f.Category, f.Scope)
	return err
}

// AddFinding attaches a finding to an existing run. A human finding is something the reviewer missed, so it is
// recorded together with the person's verdict (defect or useful, default defect); an agent finding waits for a
// person's verdict and takes none here.
func (s *Store) AddFinding(ctx context.Context, runID, origin string, f Finding, verdict, actor, note string) (StoredFinding, error) {
	if origin != "human" && origin != "agent" {
		return StoredFinding{}, fmt.Errorf("origin must be human or agent")
	}
	if origin == "agent" && verdict != "" {
		return StoredFinding{}, fmt.Errorf("an agent finding takes no verdict; a person judges it")
	}
	if origin == "human" {
		if verdict == "" {
			verdict = "defect"
		}
		v, ok := NormalizeVerdict(verdict)
		if !ok || (v != "defect" && v != "useful") {
			return StoredFinding{}, fmt.Errorf("a missed finding is defect or useful (결함|유용), got %q", verdict)
		}
		verdict = v
	}
	if f.Title == "" || f.File == "" || f.Line < 0 {
		return StoredFinding{}, fmt.Errorf("title, file and a non-negative line are required")
	}
	if !oneOf(f.Severity, "high", "medium", "low") || !oneOf(f.Confidence, "high", "medium", "low") {
		return StoredFinding{}, fmt.Errorf("severity and confidence must be high, medium or low")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return StoredFinding{}, err
	}
	defer tx.Rollback()
	var exists, ordinal int
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM runs WHERE id=?), coalesce((SELECT max(ordinal) FROM findings WHERE run_id=?),0)`,
		runID, runID).Scan(&exists, &ordinal); err != nil {
		return StoredFinding{}, err
	}
	if exists == 0 {
		return StoredFinding{}, fmt.Errorf("run %q not found", runID)
	}
	out := StoredFinding{ID: fmt.Sprintf("%s-%d", runID[:8], ordinal+1), Origin: origin, Pass: origin, Category: "defect", Finding: f}
	if err := insertFinding(ctx, tx, runID, ordinal+1, &out); err != nil {
		return StoredFinding{}, err
	}
	if origin == "human" {
		if note == "" {
			note = "missed by the reviewer"
		}
		v := Verdict{Verdict: verdict, Note: note, Actor: actor, CreatedAt: now()}
		if _, err := tx.ExecContext(ctx, `INSERT INTO verdicts(finding_id,verdict,note,actor,created_at) VALUES(?,?,?,?,?)`,
			out.ID, v.Verdict, v.Note, v.Actor, v.CreatedAt); err != nil {
			return StoredFinding{}, err
		}
		out.Verdict = &v
	}
	return out, tx.Commit()
}

func oneOf(v string, options ...string) bool {
	for _, o := range options {
		if v == o {
			return true
		}
	}
	return false
}

// useful marks a finding that is not a defect but is still a valuable review comment.
var verdictAliases = map[string]string{"defect": "defect", "useful": "useful", "false_positive": "false_positive", "unsure": "unsure",
	"duplicate": "duplicate", "결함": "defect", "유용": "useful", "오탐": "false_positive", "미확정": "unsure", "중복": "duplicate"}

const VerdictHelp = "defect|useful|false_positive|unsure|duplicate (결함|유용|오탐|미확정|중복)"

func NormalizeVerdict(v string) (string, bool) {
	n, ok := verdictAliases[v]
	return n, ok
}

func (s *Store) Judge(ctx context.Context, findingID, verdict, note, actor string) (Verdict, error) {
	v, ok := NormalizeVerdict(verdict)
	if !ok {
		return Verdict{}, fmt.Errorf("invalid verdict %q: use %s", verdict, VerdictHelp)
	}
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM findings WHERE id=?`, findingID).Scan(&exists); err != nil {
		return Verdict{}, err
	}
	if exists == 0 {
		return Verdict{}, fmt.Errorf("finding %q not found", findingID)
	}
	out := Verdict{Verdict: v, Note: note, Actor: actor, CreatedAt: now()}
	_, err := s.db.ExecContext(ctx, `INSERT INTO verdicts(finding_id,verdict,note,actor,created_at) VALUES(?,?,?,?,?)`, findingID, v, note, actor, out.CreatedAt)
	return out, err
}

func (s *Store) GetRun(ctx context.Context, id string) (Run, error) {
	var r Run
	var target, route, inspected, warnings, reviewed, checks string
	var escalated sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT id,created_at,target_kind,target_json,issue,diff,effort,route_json,escalated_from,model,observed_model,
		status,error,seconds,tool_uses,inspected_json,not_checked,warnings_json,purpose,reviewed_json,checks_json,complete FROM runs WHERE id=?`, id).Scan(
		&r.ID, &r.CreatedAt, &r.TargetKind, &target, &r.Issue, &r.Diff, &r.Effort, &route, &escalated, &r.Model, &r.ObservedModel,
		&r.Status, &r.Error, &r.Seconds, &r.ToolUses, &inspected, &r.NotChecked, &warnings, &r.Purpose, &reviewed, &checks, &r.Complete)
	if err == sql.ErrNoRows {
		return r, fmt.Errorf("run %q not found", id)
	}
	if err != nil {
		return r, err
	}
	r.EscalatedFrom = escalated.String
	_ = json.Unmarshal([]byte(target), &r.Target)
	_ = json.Unmarshal([]byte(route), &r.Route)
	_ = json.Unmarshal([]byte(inspected), &r.Inspected)
	_ = json.Unmarshal([]byte(warnings), &r.Warnings)
	_ = json.Unmarshal([]byte(reviewed), &r.Reviewed)
	_ = json.Unmarshal([]byte(checks), &r.Checks)
	rows, err := s.db.QueryContext(ctx, `SELECT f.id,f.origin,f.pass,f.category,f.scope,f.title,f.severity,f.confidence,f.file,f.line,f.condition,f.code_path,f.impact,f.evidence,
		v.verdict,v.note,v.actor,v.created_at FROM findings f LEFT JOIN verdicts v ON v.id=(SELECT max(id) FROM verdicts WHERE finding_id=f.id)
		WHERE f.run_id=? ORDER BY f.ordinal`, r.ID)
	if err != nil {
		return r, err
	}
	defer rows.Close()
	r.Findings = []StoredFinding{}
	for rows.Next() {
		var f StoredFinding
		var v, note, actor, at sql.NullString
		if err := rows.Scan(&f.ID, &f.Origin, &f.Pass, &f.Category, &f.Scope, &f.Title, &f.Severity, &f.Confidence, &f.File, &f.Line, &f.Condition, &f.CodePath, &f.Impact, &f.Evidence,
			&v, &note, &actor, &at); err != nil {
			return r, err
		}
		if v.Valid {
			f.Verdict = &Verdict{Verdict: v.String, Note: note.String, Actor: actor.String, CreatedAt: at.String}
		}
		r.Findings = append(r.Findings, f)
	}
	return r, rows.Err()
}

type RunSummary struct {
	ID         string            `json:"id"`
	CreatedAt  string            `json:"created_at"`
	TargetKind string            `json:"target_kind"`
	Target     map[string]string `json:"target"`
	Effort     string            `json:"effort"`
	Status     string            `json:"status"`
	Purpose    string            `json:"purpose"`
	Complete   bool              `json:"complete"`
	Findings   int               `json:"findings"`
	Judged     int               `json:"judged"`
}

func (s *Store) ListRuns(ctx context.Context, limit int) ([]RunSummary, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT r.id,r.created_at,r.target_kind,r.target_json,r.effort,r.status,r.purpose,r.complete,
		(SELECT count(*) FROM findings WHERE run_id=r.id),
		(SELECT count(DISTINCT v.finding_id) FROM verdicts v JOIN findings f ON f.id=v.finding_id WHERE f.run_id=r.id)
		FROM runs r ORDER BY r.created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RunSummary{}
	for rows.Next() {
		var r RunSummary
		var target string
		if err := rows.Scan(&r.ID, &r.CreatedAt, &r.TargetKind, &target, &r.Effort, &r.Status, &r.Purpose, &r.Complete, &r.Findings, &r.Judged); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(target), &r.Target)
		out = append(out, r)
	}
	return out, rows.Err()
}

// PriorFinding is an earlier finding on a file of the current change, carried forward for a re-check.
type PriorFinding struct {
	ID, RunID, File, Title, Condition, Impact, Verdict, CreatedAt string
	Line                                                          int
}

// PriorFindings returns recent findings from earlier review runs on the same remote and files, except those
// judged false positive or duplicate, newest first. With before set, only model and check findings of runs
// created before it are returned.
func (s *Store) PriorFindings(ctx context.Context, remote string, files []string, limit int, before string) ([]PriorFinding, error) {
	if remote == "" || len(files) == 0 {
		return nil, nil
	}
	want := map[string]bool{}
	for _, f := range files {
		want[strings.TrimPrefix(f, "/")] = true
	}
	rows, err := s.db.QueryContext(ctx, `SELECT f.id,f.run_id,f.file,f.line,f.title,f.condition,f.impact,r.created_at,r.target_json,
		coalesce((SELECT verdict FROM verdicts v WHERE v.finding_id=f.id ORDER BY v.id DESC LIMIT 1),'')
		FROM findings f JOIN runs r ON r.id=f.run_id WHERE r.target_json LIKE ? AND r.purpose='review'
		AND (?='' OR (r.created_at < ? AND f.origin IN ('model','check'))) ORDER BY r.created_at DESC`, "%"+remote+"%", before, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PriorFinding
	for rows.Next() && len(out) < limit {
		var p PriorFinding
		var target string
		if err := rows.Scan(&p.ID, &p.RunID, &p.File, &p.Line, &p.Title, &p.Condition, &p.Impact, &p.CreatedAt, &target, &p.Verdict); err != nil {
			return nil, err
		}
		var t map[string]string
		_ = json.Unmarshal([]byte(target), &t)
		file := strings.TrimPrefix(strings.TrimPrefix(p.File, "/"), "repo/")
		if t["remote"] != remote || !want[file] || p.Verdict == "false_positive" || p.Verdict == "duplicate" {
			continue
		}
		p.File = file
		out = append(out, p)
	}
	return out, rows.Err()
}

// CoverageRuns returns succeeded review (not targeted) runs on a remote with their reviewed file hashes.
// Runs recorded before reviewed hashes existed get them from git when their head commit is known.
func (s *Store) CoverageRuns(ctx context.Context, remote string, blobAt func(rev, path string) string) ([]Run, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,created_at,target_kind,target_json,diff,reviewed_json FROM runs
		WHERE status='succeeded' AND purpose='review' AND target_json LIKE ? ORDER BY created_at`, "%"+remote+"%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Run
	for rows.Next() {
		var r Run
		var target, reviewed string
		if err := rows.Scan(&r.ID, &r.CreatedAt, &r.TargetKind, &target, &r.Diff, &reviewed); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(target), &r.Target)
		_ = json.Unmarshal([]byte(reviewed), &r.Reviewed)
		if r.Target["remote"] != remote {
			continue
		}
		if len(r.Reviewed) == 0 && r.Target["head"] != "" && (r.TargetKind == "commit_range" || r.TargetKind == "ado_pr") {
			r.Reviewed = map[string]string{}
			for _, fd := range parseDiff([]byte(r.Diff)) {
				if fd.Deleted {
					r.Reviewed[fd.OldPath] = "deleted"
				} else if b := blobAt(r.Target["head"], fd.Path); b != "" {
					r.Reviewed[fd.Path] = b
				}
			}
		}
		r.Diff = ""
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) SaveEval(ctx context.Context, suite string, options, result any) (string, error) {
	id := uuid.NewString()
	o, _ := json.Marshal(options)
	r, _ := json.Marshal(result)
	_, err := s.db.ExecContext(ctx, `INSERT INTO evals VALUES(?,?,?,?,?)`, id, now(), suite, string(o), string(r))
	return id, err
}

// LastEval returns the previous result for a suite, or nil when there is none.
func (s *Store) LastEval(ctx context.Context, suite string) (json.RawMessage, error) {
	var r string
	err := s.db.QueryRowContext(ctx, `SELECT result_json FROM evals WHERE suite=? ORDER BY created_at DESC LIMIT 1`, suite).Scan(&r)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return json.RawMessage(r), err
}

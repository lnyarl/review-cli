package review

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/lnyarl/review-cli/internal/proc"
)

var readTools = []string{"Read", "Grep", "Glob"}

type Finding struct {
	Title      string `json:"title"`
	Severity   string `json:"severity"`
	Confidence string `json:"confidence"`
	File       string `json:"file"`
	Line       int    `json:"line"`
	Condition  string `json:"condition"`
	CodePath   string `json:"code_path"`
	Impact     string `json:"impact"`
	Evidence   string `json:"evidence"`
	// Scope is out_of_scope for a defect the issue text calls a known gap or out of scope; it is still reported.
	Scope string `json:"scope"`
}

// Output is the structured result of the defect and structure passes.
type Output struct {
	Findings       []Finding `json:"findings"`
	InspectedFiles []string  `json:"inspected_files"`
	NotChecked     string    `json:"not_checked"`
}

// Call is one model invocation in a prepared workspace. Pass names the pass so fakes and logs can tell calls apart.
type Call struct {
	Pass, WorkDir, Effort, Prompt, Schema string
}

type CallResult struct {
	Output json.RawMessage
	// Warnings lists tool calls the sandbox denied. A denied call read nothing, so the result stays valid.
	Warnings      []string
	ObservedModel string
	ToolUses      int
	Duration      time.Duration
}

// Runner runs model calls. The service depends on this so tests can fake the model.
type Runner interface {
	Call(ctx context.Context, c Call) (CallResult, error)
	Model() string
}

type ClaudeRunner struct {
	Executable string
	ModelID    string
	Timeout    time.Duration
}

func (r ClaudeRunner) Model() string { return r.ModelID }

// claudeArgs confines the reviewer: --restricted drops command tools and user/project settings and keeps
// file tools inside the workspace; only read tools are enabled and no MCP server is loaded.
func claudeArgs(model, effort, schema string) []string {
	tools := strings.Join(readTools, ",")
	return []string{"--print", "--restricted", "--tools", tools, "--allowedTools", tools, "--strict-mcp-config",
		"--setting-sources", "", "--disable-slash-commands", "--no-session-persistence", "--permission-mode", "dontAsk",
		"--json-schema", schema, "--output-format", "stream-json", "--verbose", "--model", model, "--effort", effort}
}

func (r ClaudeRunner) Call(ctx context.Context, c Call) (CallResult, error) {
	exe := r.Executable
	if !filepath.IsAbs(exe) {
		found, err := exec.LookPath(exe)
		if err != nil {
			return CallResult{}, fmt.Errorf("claude executable not found: %w", err)
		}
		if exe, err = filepath.Abs(found); err != nil {
			return CallResult{}, err
		}
	}
	out, err := proc.Run(ctx, proc.Invocation{Executable: exe, Args: claudeArgs(r.ModelID, c.Effort, c.Schema), Stdin: c.Prompt,
		WorkDir: c.WorkDir, Env: proc.BaseEnv(), Timeout: r.Timeout, MaxOutputBytes: 16 << 20})
	if err != nil {
		return CallResult{}, err
	}
	if out.ErrorCode != "" {
		return CallResult{}, fmt.Errorf("claude run failed: %s: %s", out.ErrorCode, lastLine(out.Stderr))
	}
	res, err := parseClaudeStream(out.Stdout)
	res.Duration = out.Duration
	return res, err
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}

// parseClaudeStream accepts only a session with read tools, no MCP server and a structured result.
func parseClaudeStream(stdout string) (CallResult, error) {
	var res CallResult
	allowed := map[string]bool{"StructuredOutput": true}
	for _, t := range readTools {
		allowed[t] = true
	}
	sc := bufio.NewScanner(strings.NewReader(stdout))
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	var final *json.RawMessage
	seenInit := false
	for sc.Scan() {
		var line struct {
			Type              string            `json:"type"`
			Subtype           string            `json:"subtype"`
			Model             string            `json:"model"`
			Tools             []string          `json:"tools"`
			MCPServers        []json.RawMessage `json:"mcp_servers"`
			IsError           bool              `json:"is_error"`
			PermissionDenials []json.RawMessage `json:"permission_denials"`
			StructuredOutput  *json.RawMessage  `json:"structured_output"`
			Message           struct {
				Model   string `json:"model"`
				Content []struct {
					Type string `json:"type"`
					Name string `json:"name"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &line) != nil {
			continue
		}
		switch line.Type {
		case "system":
			if line.Subtype != "init" {
				continue
			}
			seenInit = true
			res.ObservedModel = line.Model
			if len(line.MCPServers) != 0 {
				return res, fmt.Errorf("tool_scope_violation: reviewer connected an MCP server")
			}
			for _, t := range line.Tools {
				if !allowed[t] {
					return res, fmt.Errorf("tool_scope_violation: reviewer exposed tool %q", t)
				}
			}
		case "assistant":
			for _, c := range line.Message.Content {
				if c.Type == "tool_use" {
					res.ToolUses++
				}
			}
		case "result":
			if line.IsError || line.Subtype != "success" {
				return res, fmt.Errorf("cli_turn_failed: %s", line.Subtype)
			}
			for _, d := range line.PermissionDenials {
				res.Warnings = append(res.Warnings, "tool_permission_denied: "+truncate(string(d), 300))
			}
			final = line.StructuredOutput
		}
	}
	if err := sc.Err(); err != nil {
		return res, err
	}
	if !seenInit {
		return res, fmt.Errorf("invalid_cli_output: no init event")
	}
	if final == nil {
		return res, fmt.Errorf("invalid_cli_output: no structured output")
	}
	res.Output = *final
	return res, nil
}

// decodeOutput parses a defect or structure pass result.
func decodeOutput(raw json.RawMessage) (Output, error) {
	var o Output
	if err := json.Unmarshal(raw, &o); err != nil {
		return o, fmt.Errorf("invalid_cli_output: %w", err)
	}
	if o.Findings == nil {
		o.Findings = []Finding{}
	}
	if o.InspectedFiles == nil {
		o.InspectedFiles = []string{}
	}
	return o, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

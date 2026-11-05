// Package proc runs a CLI as a bounded, private child process group.
package proc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// BaseEnv preserves the local subscription login location, not arbitrary
// parent credentials, hooks or recursive CLI settings.
func BaseEnv() []string {
	keys := []string{"HOME", "USER", "LOGNAME", "PATH", "TMPDIR", "LANG", "LC_ALL", "LC_CTYPE", "SSL_CERT_FILE", "SSL_CERT_DIR", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY"}
	out := []string{}
	for _, key := range keys {
		if value, ok := os.LookupEnv(key); ok {
			out = append(out, key+"="+value)
		}
	}
	return out
}

func Prepare(inv Invocation) error {
	if !filepath.IsAbs(inv.WorkDir) || !filepath.IsAbs(inv.Executable) {
		return NewError("invalid_invocation", "absolute workspace and executable required", false)
	}
	root, err := os.Lstat(inv.WorkDir)
	if err != nil || !root.IsDir() || root.Mode()&os.ModeSymlink != 0 || root.Mode().Perm()&0077 != 0 {
		return NewError("invalid_invocation", "private workspace directory required", false)
	}
	for _, path := range inv.OutputFiles {
		if !safeOutputPath(inv.WorkDir, path) {
			return NewError("invalid_invocation", "CLI log path escapes workspace", false)
		}
	}
	for _, file := range inv.Files {
		rel, err := filepath.Rel(inv.WorkDir, file.Path)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return NewError("invalid_invocation", "configuration path escapes workspace", false)
		}
		parent := inv.WorkDir
		parts := strings.Split(rel, string(filepath.Separator))
		for _, part := range parts[:len(parts)-1] {
			parent = filepath.Join(parent, part)
			if err = os.Mkdir(parent, 0700); err != nil && !os.IsExist(err) {
				return err
			}
			info, err := os.Lstat(parent)
			if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return NewError("invalid_invocation", "configuration directory cannot be a symlink", false)
			}
		}
		f, err := os.OpenFile(file.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return NewError("invalid_invocation", "configuration file must be new and private", false)
		}
		_, err = f.Write(file.Data)
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

type outputBudget struct {
	mu        sync.Mutex
	remaining int64
	exceeded  bool
	violation string
	cancel    context.CancelFunc
}

func privateOutputPath(root, path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func safeOutputPath(root, path string) bool {
	if !privateOutputPath(root, path) {
		return false
	}
	rel, _ := filepath.Rel(root, path)
	current := root
	parts := strings.Split(rel, string(filepath.Separator))
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return true
		}
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
		if i < len(parts)-1 && !info.IsDir() || i == len(parts)-1 && !info.Mode().IsRegular() {
			return false
		}
	}
	return true
}
func outputFilesSize(inv Invocation) (int64, string) {
	var total int64
	for _, path := range inv.OutputFiles {
		if !safeOutputPath(inv.WorkDir, path) {
			return 0, "output_scope_violation"
		}
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return 0, "output_scope_violation"
		}
		total += info.Size()
	}
	return total, ""
}
func checkOutputFiles(inv Invocation, budget *outputBudget) {
	size, violation := outputFilesSize(inv)
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if violation != "" {
		budget.violation = violation
		budget.cancel()
	}
	if size > budget.remaining {
		budget.exceeded = true
		budget.cancel()
	}
}
func sanitizeOutputFiles(inv Invocation) error {
	for _, path := range inv.OutputFiles {
		if !safeOutputPath(inv.WorkDir, path) {
			return NewError("output_scope_violation", "CLI log path is not private", false)
		}
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return NewError("output_scope_violation", "CLI log cannot be read safely", false)
		}
		if err = os.WriteFile(path, []byte(Redact(string(data), inv.Secrets)), 0600); err != nil {
			return err
		}
		if err = os.Chmod(path, 0600); err != nil {
			return err
		}
	}
	return nil
}

type boundedOutput struct {
	budget *outputBudget
	data   bytes.Buffer
}

func (w *boundedOutput) Write(p []byte) (int, error) {
	w.budget.mu.Lock()
	defer w.budget.mu.Unlock()
	n := min(int64(len(p)), w.budget.remaining)
	if n > 0 {
		_, _ = w.data.Write(p[:n])
		w.budget.remaining -= n
	}
	if n < int64(len(p)) {
		w.budget.exceeded = true
		w.budget.cancel()
		return int(n), io.ErrShortWrite
	}
	return len(p), nil
}

func Redact(text string, secrets []string) string {
	ordered := append([]string{}, secrets...)
	sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	for _, secret := range ordered {
		if secret == "" {
			continue
		}
		text = strings.ReplaceAll(text, secret, "[REDACTED]")
		// A bounded log can end in the middle of a credential.
		for n := len(secret) - 1; n >= 4; n-- {
			if strings.HasSuffix(text, secret[:n]) {
				text = strings.TrimSuffix(text, secret[:n]) + "[REDACTED]"
				break
			}
		}
	}
	return text
}

// Run creates no shell. Its process group includes spawned children, so
// cancellation, output overflow and a normal parent exit cannot orphan them.
func Run(ctx context.Context, inv Invocation) (ProcessResult, error) {
	out := ProcessResult{ExitCode: -1, WorkDir: inv.WorkDir}
	if inv.Timeout <= 0 || inv.Timeout > time.Hour {
		return out, NewError("invalid_invocation", "bounded timeout required", false)
	}
	if err := Prepare(inv); err != nil {
		return out, err
	}
	limit := inv.MaxOutputBytes
	if limit <= 0 {
		limit = 2 << 20
	}
	if limit > 16<<20 {
		return out, NewError("invalid_invocation", "output limit exceeds 16 MiB", false)
	}
	runCtx, cancel := context.WithTimeout(ctx, inv.Timeout)
	defer cancel()
	budget := &outputBudget{remaining: limit, cancel: cancel}
	stdout, stderr := &boundedOutput{budget: budget}, &boundedOutput{budget: budget}
	cmd := exec.CommandContext(runCtx, inv.Executable, inv.Args...)
	cmd.Dir, cmd.Env = inv.WorkDir, append([]string{}, inv.Env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = strings.NewReader(inv.Stdin), stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process != nil {
			return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
		return nil
	}
	cmd.WaitDelay = 2 * time.Second
	started := time.Now()
	err := cmd.Start()
	if err != nil {
		out.ErrorCode = "executable_unavailable"
		return out, NewError(out.ErrorCode, "CLI could not start", false)
	}
	watchCtx, stopWatch := context.WithCancel(runCtx)
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-watchCtx.Done():
				return
			case <-ticker.C:
				checkOutputFiles(inv, budget)
			}
		}
	}()
	err = cmd.Wait()
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	stopWatch()
	<-watchDone
	checkOutputFiles(inv, budget)
	out.Duration, out.ExitCode = time.Since(started), cmd.ProcessState.ExitCode()
	out.Stdout, out.Stderr = Redact(stdout.data.String(), inv.Secrets), Redact(stderr.data.String(), inv.Secrets)
	switch {
	case budget.violation != "":
		out.ErrorCode = budget.violation
	case budget.exceeded:
		out.ErrorCode = "output_limit"
	case errors.Is(runCtx.Err(), context.DeadlineExceeded):
		out.ErrorCode = "timeout"
	case ctx.Err() != nil:
		out.ErrorCode = "canceled"
	case err != nil:
		out.ErrorCode = "cli_failed"
	}
	if out.ErrorCode == "output_limit" && budget.violation == "" {
		// Oversized files must not retain an unredacted truncated credential.
		for _, path := range inv.OutputFiles {
			info, err := os.Lstat(path)
			if os.IsNotExist(err) {
				continue
			}
			if err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
				_ = os.WriteFile(path, []byte("[CLI log removed after output limit]\n"), 0600)
				_ = os.Chmod(path, 0600)
			}
		}
	}
	if out.ErrorCode != "output_limit" && out.ErrorCode != "output_scope_violation" {
		if err := sanitizeOutputFiles(inv); err != nil {
			out.ErrorCode = "output_scope_violation"
		}
	}
	if out.ErrorCode != "" {
		return out, NewError(out.ErrorCode, "CLI execution did not complete successfully", out.ErrorCode == "timeout" || out.ErrorCode == "cli_failed")
	}
	return out, nil
}

type File struct {
	Path string
	Data []byte
}

type Invocation struct {
	Executable     string
	Args           []string
	Stdin          string
	WorkDir        string
	Env            []string
	Files          []File
	OutputFiles    []string
	Secrets        []string
	Timeout        time.Duration
	MaxOutputBytes int64
}

type ProcessResult struct {
	WorkDir   string        `json:"work_dir"`
	ExitCode  int           `json:"exit_code"`
	Stdout    string        `json:"stdout"`
	Stderr    string        `json:"stderr"`
	Duration  time.Duration `json:"duration"`
	ErrorCode string        `json:"error_code,omitempty"`
}

type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func (e *Error) Error() string                            { return e.Code + ": " + e.Message }
func NewError(code, message string, retryable bool) error { return &Error{code, message, retryable} }

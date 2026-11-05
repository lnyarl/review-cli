package review

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func git(ctx context.Context, repo string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", repo}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// gitDiffNoIndex returns a new-file diff; --no-index exits 1 when the files differ.
func gitDiffNoIndex(ctx context.Context, repo, path string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "diff", "--no-index", "--", "/dev/null", path)
	out, err := cmd.Output()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return out, nil
	}
	return out, err
}

func resolveCommit(ctx context.Context, repo, rev string) (string, error) {
	out, err := git(ctx, repo, "rev-parse", "--verify", rev+"^{commit}")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func remoteURL(ctx context.Context, repo string) string {
	out, err := git(ctx, repo, "remote", "get-url", "origin")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// exportCommit writes the tree of commit into dst without .git, so the reviewer cannot read later history.
// Symlinks are skipped: they could point outside the workspace.
func exportCommit(ctx context.Context, repo, commit, dst string) error {
	archive, err := git(ctx, repo, "archive", "--format=tar", commit)
	if err != nil {
		return err
	}
	tr := tar.NewReader(bytes.NewReader(archive))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(h.Name)
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") {
			return fmt.Errorf("unsafe archive path %q", h.Name)
		}
		path := filepath.Join(dst, name)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o700); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
		}
	}
}

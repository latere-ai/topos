package input

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// DiffSpec parameterizes a single diff invocation.
type DiffSpec struct {
	From string
	To   string
	Cwd  string
}

// Diff is the result of Compute.
type Diff struct {
	Patch        string
	ChangedLines int
	Files        []string
}

// ErrNotGitRepo is returned when Cwd is not inside a git work tree.
var ErrNotGitRepo = errors.New("not a git repo")

// ErrGit wraps an underlying git invocation failure.
type ErrGit struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *ErrGit) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.Args, " "), e.Err, strings.TrimSpace(e.Stderr))
}

// Compute runs `git diff` and returns *Diff.
//
//	From="HEAD", To="."  -> working tree vs HEAD (incl. untracked)
//	From=A,      To=B    -> committed range
func Compute(ctx context.Context, s DiffSpec) (*Diff, error) {
	if err := requireGitRepo(ctx, s.Cwd); err != nil {
		return nil, err
	}

	var args []string
	if s.To == "." {
		args = []string{"diff", "--no-color", s.From}
	} else {
		args = []string{"diff", "--no-color", s.From, s.To}
	}
	tracked, err := runGit(ctx, s.Cwd, args)
	if err != nil {
		return nil, err
	}

	var patch bytes.Buffer
	patch.WriteString(tracked)

	// Append synthetic diffs for untracked files when To == "."
	if s.To == "." {
		untracked, err := runGit(ctx, s.Cwd, []string{"ls-files", "--others", "--exclude-standard"})
		if err != nil {
			return nil, err
		}
		for _, p := range strings.Split(strings.TrimSpace(untracked), "\n") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			synth, err := runGit(ctx, s.Cwd, []string{"diff", "--no-color", "--no-index", "--", "/dev/null", p})
			if err != nil {
				// git diff --no-index exits 1 (with output) when the files
				// differ; that is the expected case for an untracked file,
				// not a failure. Any other error (e.g. git could not read
				// the file) is genuine and must propagate, rather than be
				// swallowed and undercount ChangedLines.
				var ge *ErrGit
				if errors.As(err, &ge) && len(synth) > 0 {
					patch.WriteString(synth)
					continue
				}
				return nil, err
			}
			patch.WriteString(synth)
		}
	}

	d := &Diff{Patch: patch.String()}
	d.ChangedLines, d.Files = analyzePatch(d.Patch)
	return d, nil
}

// Trivial returns true iff d.ChangedLines < threshold.
func Trivial(d *Diff, threshold int) bool {
	return d != nil && d.ChangedLines < threshold
}

func requireGitRepo(ctx context.Context, cwd string) error {
	out, err := runGit(ctx, cwd, []string{"rev-parse", "--is-inside-work-tree"})
	if err != nil {
		return ErrNotGitRepo
	}
	if strings.TrimSpace(out) != "true" {
		return ErrNotGitRepo
	}
	return nil
}

func runGit(ctx context.Context, cwd string, args []string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = cwd
	cmd.Env = append(cmd.Environ(), "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		// `git diff --no-index` exits 1 when files differ - that's data,
		// not failure. The caller decides how to interpret.
		return string(out), &ErrGit{Args: args, Stderr: stderr.String(), Err: err}
	}
	return string(out), nil
}

func analyzePatch(patch string) (int, []string) {
	var changed int
	files := map[string]struct{}{}
	// inHunk distinguishes hunk-body +/- lines from file-header lines.
	// Without it a content line whose text starts with "-- " or "++ "
	// (emitted by git as "--- ..." / "+++ ...") is mistaken for a
	// /dev/null header and dropped, undercounting ChangedLines. Each
	// "diff --git" resets the flag so a later file's headers, which
	// arrive after a previous file's hunk, are not counted as changes.
	inHunk := false
	for _, line := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git"):
			inHunk = false
		case strings.HasPrefix(line, "@@"):
			inHunk = true
		case inHunk && (strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-")):
			changed++
		case strings.HasPrefix(line, "+++ b/"):
			files[strings.TrimPrefix(line, "+++ b/")] = struct{}{}
		case strings.HasPrefix(line, "--- a/"):
			files[strings.TrimPrefix(line, "--- a/")] = struct{}{}
		}
	}
	out := make([]string, 0, len(files))
	for f := range files {
		out = append(out, f)
	}
	return changed, out
}

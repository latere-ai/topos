// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/prompts"
	"latere.ai/x/topos/session"
)

// Limits of spec 011.
const (
	maxInstructionFile  = 64 << 10
	maxInstructionTotal = 256 << 10
	maxSkills           = 100
	maxSkillDescription = 1024
)

// instructionNames are read in each directory, in this order.
var instructionNames = []string{"AGENTS.md", "CLAUDE.md"}

// skillDirs are the repository's skill folders, in order.
var skillDirs = []string{".agents/skills", ".claude/skills"}

var skillName = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

// Attachment is what attaching a machine records: the context block, the
// instruction files and the skills index of spec 011.
type Attachment struct {
	Machine      session.AttachedMachine
	Context      string
	Instructions []session.Instructions
	Skills       []session.Skill
}

// attachOptions are the person's own sources on the host.
type attachOptions struct {
	// PersonalInstructions is the person's own AGENTS.md, read on the
	// host before the repository's files.
	PersonalInstructions string
	// PersonalSkills is the person's own skills folder on the host.
	PersonalSkills string
	Now            time.Time
}

// attach computes a machine's attachment, storing each instruction file
// as a blob of the session.
func attach(ctx context.Context, m machine.Machine, l *Log, o attachOptions) (Attachment, error) {
	info := m.Info()
	a := Attachment{Machine: session.AttachedMachine{Kind: info.Kind, ID: info.ID, Workdir: info.Workdir, OS: info.OS, Arch: info.Arch, Environment: info.Environment, Sandbox: info.Sandbox, Egress: info.Egress}}
	top, inRepo := gitTop(ctx, m)
	root := info.Workdir
	if inRepo {
		root = top
	}
	a.Context = contextBlock(ctx, m, info, inRepo, o.Now)

	total := 0
	add := func(p string, b []byte) error {
		if total >= maxInstructionTotal {
			return nil
		}
		if len(b) > maxInstructionFile {
			b = append(b[:maxInstructionFile:maxInstructionFile], "\n"+prompts.Text(prompts.InstructionCut)...)
		}
		if room := maxInstructionTotal - total; len(b) > room {
			b = append(b[:room:room], "\n"+prompts.Text(prompts.InstructionsTotalCut)...)
		}
		total += len(b)
		d, err := l.PutBlob(ctx, bytes.NewReader(b))
		if err != nil {
			return fmt.Errorf("runner: store the instructions of %s: %w", p, err)
		}
		a.Instructions = append(a.Instructions, session.Instructions{Path: p, SHA256: d.Hex(), Blob: d})
		return nil
	}
	if info.Kind == machine.KindHost && o.PersonalInstructions != "" {
		b, err := os.ReadFile(o.PersonalInstructions)
		switch {
		case err == nil:
			if err := add(o.PersonalInstructions, b); err != nil {
				return Attachment{}, err
			}
		case !absent(err):
			return Attachment{}, fmt.Errorf("runner: read %s: %w", o.PersonalInstructions, err)
		}
	}
	for _, dir := range chain(root, info.Workdir) {
		for _, name := range instructionNames {
			p := path.Join(dir, name)
			b, ok, err := readIfPresent(ctx, m, p)
			if err != nil {
				return Attachment{}, err
			}
			if ok {
				if err := add(p, b); err != nil {
					return Attachment{}, err
				}
			}
		}
	}

	seen := map[string]bool{}
	for _, d := range skillDirs {
		skills, err := machineSkills(ctx, m, path.Join(root, d))
		if err != nil {
			return Attachment{}, err
		}
		a.Skills = appendSkills(a.Skills, skills, seen)
	}
	if info.Kind == machine.KindHost && o.PersonalSkills != "" {
		skills, err := localSkills(o.PersonalSkills)
		if err != nil {
			return Attachment{}, err
		}
		a.Skills = appendSkills(a.Skills, skills, seen)
	}
	return a, nil
}

func appendSkills(dst, src []session.Skill, seen map[string]bool) []session.Skill {
	for _, s := range src {
		if len(dst) == maxSkills || seen[s.Name] {
			continue
		}
		seen[s.Name] = true
		dst = append(dst, s)
	}
	return dst
}

// chain is the directories from root down to dir, both included; dir
// outside root is only itself.
func chain(root, dir string) []string {
	root, dir = path.Clean(root), path.Clean(dir)
	if dir != root && !strings.HasPrefix(dir, root+"/") {
		return []string{dir}
	}
	out := []string{root}
	rest := strings.TrimPrefix(strings.TrimPrefix(dir, root), "/")
	cur := root
	for seg := range strings.SplitSeq(rest, "/") {
		if seg == "" {
			continue
		}
		cur = path.Join(cur, seg)
		out = append(out, cur)
	}
	return out
}

// readIfPresent reads a file of the machine, reporting false for one
// that does not exist or that the machine keeps out of reach.
func readIfPresent(ctx context.Context, m machine.Machine, p string) ([]byte, bool, error) {
	rc, err := m.ReadFile(ctx, p)
	if absent(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("runner: read %s: %w", p, err)
	}
	b, rerr := io.ReadAll(io.LimitReader(rc, maxInstructionFile+1))
	if cerr := rc.Close(); rerr == nil {
		rerr = cerr
	}
	if rerr != nil {
		return nil, false, fmt.Errorf("runner: read %s: %w", p, rerr)
	}
	return b, true, nil
}

// machineSkills lists the skills of one folder on the machine.
func machineSkills(ctx context.Context, m machine.Machine, dir string) ([]session.Skill, error) {
	entries, err := m.List(ctx, dir)
	if absent(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("runner: list %s: %w", dir, err)
	}
	var out []session.Skill
	for _, e := range entries {
		if !e.IsDir {
			continue
		}
		p := path.Join(e.Path, "SKILL.md")
		b, ok, err := readIfPresent(ctx, m, p)
		if err != nil {
			return nil, err
		}
		if s, valid := parseSkill(b, p); ok && valid {
			out = append(out, s)
		}
	}
	return out, nil
}

// localSkills lists the person's own skills from the runner's disk.
func localSkills(dir string) ([]session.Skill, error) {
	entries, err := os.ReadDir(dir)
	if absent(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("runner: list %s: %w", dir, err)
	}
	var out []session.Skill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := path.Join(dir, e.Name(), "SKILL.md")
		b, err := os.ReadFile(p)
		if absent(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("runner: read %s: %w", p, err)
		}
		if s, valid := parseSkill(b, p); valid {
			out = append(out, s)
		}
	}
	return out, nil
}

// absent reports an error that means the file is not there for the
// session: it does not exist, or the machine keeps it out of reach.
func absent(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, machine.ErrOutside) || errors.Is(err, machine.ErrDenied)
}

// parseSkill reads the name and description of a SKILL.md's frontmatter.
func parseSkill(b []byte, p string) (session.Skill, bool) {
	text := strings.ReplaceAll(string(b), "\r\n", "\n")
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return session.Skill{}, false
	}
	front, _, ok := strings.Cut(rest, "\n---")
	if !ok {
		return session.Skill{}, false
	}
	s := session.Skill{Path: p}
	for line := range strings.SplitSeq(front, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		switch strings.TrimSpace(k) {
		case "name":
			s.Name = v
		case "description":
			s.Description = v
		}
	}
	if !skillName.MatchString(s.Name) || s.Description == "" || len(s.Description) > maxSkillDescription {
		return session.Skill{}, false
	}
	return s, true
}

// git runs a git command on the machine and returns its trimmed output,
// or false when git is missing or the command failed.
func git(ctx context.Context, m machine.Machine, args string) (string, bool) {
	res, err := m.Exec(ctx, machine.ExecRequest{Command: "git " + args, Timeout: 10 * time.Second})
	if err != nil || res.ExitCode != 0 {
		return "", false
	}
	return strings.TrimSpace(string(res.Output)), true
}

func gitTop(ctx context.Context, m machine.Machine) (string, bool) {
	return git(ctx, m, "rev-parse --show-toplevel")
}

// contextBlock is the context block of spec 011.
func contextBlock(ctx context.Context, m machine.Machine, info machine.Info, inRepo bool, now time.Time) string {
	var branch, head string
	var modified, untracked int
	var commits []string
	if inRepo {
		branch, _ = git(ctx, m, "rev-parse --abbrev-ref HEAD")
		head, _ = git(ctx, m, "rev-parse --short HEAD")
		status, _ := git(ctx, m, "status --porcelain")
		for line := range strings.SplitSeq(status, "\n") {
			switch {
			case line == "":
			case strings.HasPrefix(line, "??"):
				untracked++
			default:
				modified++
			}
		}
		if log, ok := git(ctx, m, "log -5 --format='%h %s'"); ok && log != "" {
			commits = strings.Split(log, "\n")
		}
	}
	return prompts.Render(prompts.ContextBlock, prompts.Data{
		"Workdir": info.Workdir, "OS": info.OS, "Arch": info.Arch,
		"Cella": info.Kind == machine.KindCella, "Environment": info.Environment,
		"Date": now.UTC().Format("2006-01-02"), "Git": inRepo, "Branch": branch, "Head": head,
		"Modified": modified, "Untracked": untracked, "Commits": commits,
	})
}

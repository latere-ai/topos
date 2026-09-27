// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package machine

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Search kinds.
const (
	SearchGrep = "grep"
	SearchGlob = "glob"
)

// Grep output modes.
const (
	ModeFiles   = "files_with_matches"
	ModeContent = "content"
	ModeCount   = "count"
)

// Limits of spec 008.
const (
	DefaultHeadLimit = 250
	GlobLimit        = 1000
	// binarySniff is how many leading bytes decide whether a file is
	// binary: one NUL among them is.
	binarySniff = 8000
)

// SearchRequest is a grep or a glob.
type SearchRequest struct {
	Kind    string
	Pattern string
	// Path is where the search starts; empty is the working directory.
	Path            string
	Glob            string
	OutputMode      string
	Context         int
	CaseInsensitive bool
	Multiline       bool
	HeadLimit       int
}

// SearchResult is the search's output: one line per path or match, as
// the tool renders it.
type SearchResult struct {
	Lines     []string
	Truncated bool
}

// SearchFS runs q over fsys, whose names are relative to base, the
// absolute path fsys is rooted at. rel is where under fsys the search
// starts, "." for the root. The walk honors .gitignore files, skips .git
// and, for grep, binary files; paths in the result are absolute.
func SearchFS(ctx context.Context, fsys fs.FS, base, rel string, q SearchRequest) (SearchResult, error) {
	switch q.Kind {
	case SearchGrep:
		return grep(ctx, fsys, base, rel, q)
	case SearchGlob:
		return glob(ctx, fsys, base, rel, q)
	}
	return SearchResult{}, fmt.Errorf("machine: search kind %q is not grep or glob", q.Kind)
}

// walk visits the files under rel that no .gitignore excludes, in
// lexical order.
func walk(ctx context.Context, fsys fs.FS, rel string, visit func(name string, d fs.DirEntry) error) error {
	ig := &ignores{}
	if err := ig.loadParents(fsys, rel); err != nil {
		return err
	}
	return fs.WalkDir(fsys, rel, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			if name == rel {
				return err
			}
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" && name != rel {
				return fs.SkipDir
			}
			if name != rel && ig.ignored(name, true) {
				return fs.SkipDir
			}
			return ig.load(fsys, name)
		}
		if ig.ignored(name, false) {
			return nil
		}
		return visit(name, d)
	})
}

func absolute(base, name string) string {
	if name == "." {
		return base
	}
	return path.Join(base, name)
}

func grep(ctx context.Context, fsys fs.FS, base, rel string, q SearchRequest) (SearchResult, error) {
	expr := q.Pattern
	flags := ""
	if q.CaseInsensitive {
		flags += "i"
	}
	if q.Multiline {
		flags += "s"
	}
	if flags != "" {
		expr = "(?" + flags + ")" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return SearchResult{}, fmt.Errorf("machine: the pattern is not a valid RE2 expression: %w", err)
	}
	mode := cmp.Or(q.OutputMode, ModeFiles)
	switch mode {
	case ModeFiles, ModeContent, ModeCount:
	default:
		return SearchResult{}, fmt.Errorf("machine: output mode %q is not files_with_matches, content or count", mode)
	}
	limit := q.HeadLimit
	if limit <= 0 {
		limit = DefaultHeadLimit
	}
	var res SearchResult
	full := errors.New("full")
	add := func(line string) error {
		if len(res.Lines) == limit {
			res.Truncated = true
			return full
		}
		res.Lines = append(res.Lines, line)
		return nil
	}
	err = walk(ctx, fsys, rel, func(name string, d fs.DirEntry) error {
		if q.Glob != "" && !matchFileGlob(q.Glob, relTo(rel, name)) {
			return nil
		}
		b, ok := readText(fsys, name)
		if !ok {
			return nil
		}
		file := absolute(base, name)
		switch mode {
		case ModeFiles:
			if re.Match(b) {
				return add(file)
			}
		case ModeCount:
			if n := countMatches(re, b, q.Multiline); n > 0 {
				return add(file + ":" + strconv.Itoa(n))
			}
		case ModeContent:
			for _, l := range contentLines(re, b, q.Context, q.Multiline) {
				line := "--"
				if !l.separator {
					line = file + l.text
				}
				if err := add(line); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, full) {
		return SearchResult{}, err
	}
	return res, nil
}

// readText returns a file's content, or false for a file that cannot be
// read or is binary; grep passes over both.
func readText(fsys fs.FS, name string) ([]byte, bool) {
	b, err := fs.ReadFile(fsys, name)
	if err != nil || bytes.IndexByte(b[:min(len(b), binarySniff)], 0) >= 0 {
		return nil, false
	}
	return b, true
}

// modTime returns an entry's modification time, or false for an entry
// that vanished during the walk; glob passes over it.
func modTime(d fs.DirEntry) (int64, bool) {
	info, err := d.Info()
	if err != nil {
		return 0, false
	}
	return info.ModTime().UnixNano(), true
}

func relTo(rel, name string) string {
	if rel == "." {
		return name
	}
	return strings.TrimPrefix(strings.TrimPrefix(name, rel), "/")
}

func countMatches(re *regexp.Regexp, b []byte, multiline bool) int {
	if multiline {
		return len(re.FindAllIndex(b, -1))
	}
	n := 0
	for l := range bytes.SplitSeq(b, []byte("\n")) {
		if re.Match(l) {
			n++
		}
	}
	return n
}

// contentLine is one line of content output: a line of the file, or the
// separator between groups that do not touch.
type contentLine struct {
	separator bool
	text      string
}

// contentLines renders the matching lines of b as ":<n>:<text>" and
// context lines as "-<n>-<text>", for the caller to prefix with the file.
// A multiline match marks every line it spans.
func contentLines(re *regexp.Regexp, b []byte, context int, multiline bool) []contentLine {
	lines := strings.Split(string(b), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	match := make([]bool, len(lines))
	found := false
	if multiline {
		starts := lineStarts(b)
		for _, loc := range re.FindAllIndex(b, -1) {
			first := lineOf(starts, loc[0])
			last := lineOf(starts, max(loc[1]-1, loc[0]))
			for i := first; i <= last && i < len(match); i++ {
				match[i], found = true, true
			}
		}
	} else {
		for i, l := range lines {
			if re.MatchString(l) {
				match[i], found = true, true
			}
		}
	}
	if !found {
		return nil
	}
	var out []contentLine
	last := -1
	for i := range lines {
		near := false
		for j := max(0, i-context); j <= min(len(lines)-1, i+context); j++ {
			if match[j] {
				near = true
				break
			}
		}
		if !near {
			continue
		}
		if last >= 0 && i > last+1 {
			out = append(out, contentLine{separator: true})
		}
		sep := "-"
		if match[i] {
			sep = ":"
		}
		out = append(out, contentLine{text: sep + strconv.Itoa(i+1) + sep + lines[i]})
		last = i
	}
	return out
}

func lineStarts(b []byte) []int {
	starts := []int{0}
	for i, c := range b {
		if c == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

func lineOf(starts []int, off int) int {
	i, found := slices.BinarySearch(starts, off)
	if found {
		return i
	}
	return i - 1
}

func glob(ctx context.Context, fsys fs.FS, base, rel string, q SearchRequest) (SearchResult, error) {
	if q.Pattern == "" {
		return SearchResult{}, errors.New("machine: glob needs a pattern")
	}
	if _, err := path.Match(strings.ReplaceAll(q.Pattern, "**", "*"), ""); err != nil {
		return SearchResult{}, fmt.Errorf("machine: the glob pattern: %w", err)
	}
	type hit struct {
		name string
		mod  int64
	}
	var hits []hit
	err := walk(ctx, fsys, rel, func(name string, d fs.DirEntry) error {
		if !matchGlob(q.Pattern, relTo(rel, name)) {
			return nil
		}
		if mod, ok := modTime(d); ok {
			hits = append(hits, hit{name, mod})
		}
		return nil
	})
	if err != nil {
		return SearchResult{}, err
	}
	slices.SortStableFunc(hits, func(a, b hit) int {
		if c := cmp.Compare(b.mod, a.mod); c != 0 {
			return c
		}
		return strings.Compare(a.name, b.name)
	})
	var res SearchResult
	for i, h := range hits {
		if i == GlobLimit {
			res.Truncated = true
			break
		}
		res.Lines = append(res.Lines, absolute(base, h.name))
	}
	return res, nil
}

// matchFileGlob matches a grep glob: a pattern without a slash matches
// the base name at any depth, one with a slash the path.
func matchFileGlob(pattern, name string) bool {
	if !strings.Contains(pattern, "/") {
		return matchGlob(pattern, path.Base(name))
	}
	return matchGlob(pattern, name)
}

// matchGlob matches name against a slash-separated pattern in which
// "**" is any number of whole segments, zero included.
func matchGlob(pattern, name string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			rest := pat[1:]
			for i := 0; i <= len(name); i++ {
				if matchSegments(rest, name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], name[0]); err != nil || !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

// ignores is the .gitignore rules in force at a point of the walk.
type ignores struct {
	rules []ignoreRule
}

type ignoreRule struct {
	base     string // the directory of the .gitignore, "." for the root
	pattern  string
	negate   bool
	dirOnly  bool
	anchored bool
}

// loadParents reads the .gitignore files of rel's ancestors, so a
// search that starts below the root still honors them.
func (ig *ignores) loadParents(fsys fs.FS, rel string) error {
	if rel == "." {
		return nil
	}
	dir := "."
	if err := ig.load(fsys, dir); err != nil {
		return err
	}
	for seg := range strings.SplitSeq(path.Dir(rel), "/") {
		if seg == "." {
			continue
		}
		dir = path.Join(dir, seg)
		if err := ig.load(fsys, dir); err != nil {
			return err
		}
	}
	return nil
}

// load reads dir's .gitignore; a directory without one adds no rules.
func (ig *ignores) load(fsys fs.FS, dir string) (err error) {
	f, oerr := fsys.Open(path.Join(dir, ".gitignore"))
	if oerr != nil {
		return nil
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	sc := bufio.NewScanner(io.LimitReader(f, 1<<20))
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), " \t\r")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		r := ignoreRule{base: dir}
		if strings.HasPrefix(line, "!") {
			r.negate, line = true, line[1:]
		} else if strings.HasPrefix(line, `\`) {
			line = line[1:]
		}
		if l, ok := strings.CutSuffix(line, "/"); ok {
			r.dirOnly, line = true, l
		}
		if strings.Contains(line, "/") {
			r.anchored, line = true, strings.TrimPrefix(line, "/")
		}
		if line == "" {
			continue
		}
		r.pattern = line
		ig.rules = append(ig.rules, r)
	}
	return sc.Err()
}

// ignored reports whether name is excluded: the last rule that matches
// decides.
func (ig *ignores) ignored(name string, isDir bool) bool {
	out := false
	for _, r := range ig.rules {
		if r.dirOnly && !isDir {
			continue
		}
		rel := name
		if r.base != "." {
			if !strings.HasPrefix(name, r.base+"/") {
				continue
			}
			rel = name[len(r.base)+1:]
		}
		var ok bool
		if r.anchored {
			ok = matchGlob(r.pattern, rel)
		} else {
			ok = matchGlob(r.pattern, path.Base(rel))
		}
		if ok {
			out = !r.negate
		}
	}
	return out
}

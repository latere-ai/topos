// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package prompts holds every text topos writes for a model to read: the
// harness prompt and its sections, the compaction prompt, the advisor's
// instructions and request, the tool descriptions, the texts the session
// fold writes into a transcript, the results of calls and tools, the
// rendering of the context block, instruction files, skills and memory
// stores, and the reminders the harness adds to one request (specs 004,
// 008, 010, 011, 013 and 062).
//
// Each text is one file embedded in the build, named by its path under
// prompts/ without the extension and ending in its version, such as
// results/files/changed-v1. A static text is plain; a dynamic one is a
// text/template rendered with the values it names. A change of wording
// is a new file with the next version, never an edit of a released one,
// so a replay can rebuild an old request from the versions it recorded.
// Rendering reads nothing but the embedded files and its arguments, so
// the same inputs give the same bytes on every run.
package prompts

import (
	"embed"
	"fmt"
	"io/fs"
	"path"
	"strings"
	"text/template"
	"text/template/parse"
)

//go:embed harness compact advisor tools transcript results context reminders
var files embed.FS

// Name is the stable name of one text at one version: its file's path
// under prompts/ without the .md extension.
type Name string

// Data are the values a dynamic text is rendered with, by the names its
// template uses. A key the template uses and Data lacks fails the render.
type Data map[string]any

// catalog is every embedded text: the parsed templates, and the file's
// text of each one that has no template action.
type catalog struct {
	set    *template.Template
	static map[string]string
}

var texts = mustLoad(files)

func mustLoad(fsys fs.FS) catalog {
	c, err := load(fsys)
	if err != nil {
		panic(err)
	}
	return c
}

// load parses every .md file of fsys. A file's text is its content with
// the surrounding whitespace removed, so the newline a file ends with is
// not part of the text; whitespace a caller needs around a text is the
// caller's.
func load(fsys fs.FS) (catalog, error) {
	c := catalog{set: template.New("").Option("missingkey=error"), static: map[string]string{}}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if path.Ext(p) != ".md" {
			return fmt.Errorf("prompts: %s is not a .md file", p)
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return fmt.Errorf("prompts: read %s: %w", p, err)
		}
		name := strings.TrimSuffix(p, ".md")
		body := strings.TrimSpace(string(b))
		t, err := c.set.New(name).Parse(body)
		if err != nil {
			return fmt.Errorf("prompts: parse %s: %w", p, err)
		}
		if plain(t.Root) {
			c.static[name] = body
		}
		return nil
	})
	return c, err
}

// plain reports whether a parsed file holds only text.
func plain(root *parse.ListNode) bool {
	for _, n := range root.Nodes {
		if n.Type() != parse.NodeText {
			return false
		}
	}
	return true
}

// Execute renders the text n with data, and fails for a name that no
// file holds or a value the template uses and data lacks.
func Execute(n Name, data Data) (string, error) {
	return texts.execute(n, data)
}

func (c catalog) execute(n Name, data Data) (string, error) {
	t := c.set.Lookup(string(n))
	if t == nil {
		return "", fmt.Errorf("prompts: no text %s", n)
	}
	var b strings.Builder
	if err := t.Execute(&b, data); err != nil {
		return "", fmt.Errorf("prompts: render %s: %w", n, err)
	}
	return b.String(), nil
}

// Render renders the text n with data. The names callers pass are
// constants of this package, and its tests render every one with the
// keys each caller passes, so a failure is a defect of the build, not of
// a run: Render panics on it the way template.Must does.
func Render(n Name, data Data) string {
	s, err := Execute(n, data)
	if err != nil {
		panic(err)
	}
	return s
}

// Text returns the static text n. It panics when no file holds n or the
// file is a template, a defect the package's tests rule out.
func Text(n Name) string {
	s, ok := texts.static[string(n)]
	if !ok {
		panic(fmt.Sprintf("prompts: no static text %s", n))
	}
	return s
}

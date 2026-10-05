// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/prompts"
)

const writeSchema = `{
  "type": "object",
  "properties": {
    "path": {"type": "string", "minLength": 1, "description": "The file to write: absolute, or relative to the working directory."},
    "content": {"type": "string", "description": "The whole new content of the file."}
  },
  "required": ["path", "content"],
  "additionalProperties": false
}`

func writeTool() Tool {
	return newBuiltin(NameWrite, prompts.Text(prompts.ToolWrite), writeSchema, Properties{Effect: EffectWrite}, runWrite)
}

type writeInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func runWrite(ctx context.Context, b *builtin, c Call) (Result, error) {
	var in writeInput
	if r := b.decode(c, &in); r != nil {
		return *r, nil
	}
	p := resolve(c.Machine, in.Path)
	f, err := current(ctx, c, p)
	if err != nil {
		return b.fail(ctx, c, p, err)
	}
	if f.refusal != "" {
		return b.result(ctx, c, OutcomeError, f.refusal, nil)
	}
	if err := c.Machine.WriteFile(ctx, p, strings.NewReader(in.Content), 0); err != nil {
		return b.fail(ctx, c, p, err)
	}
	size := int64(len(in.Content))
	meta := &Meta{Path: p, SHA256: digest([]byte(in.Content)), Size: &size}
	done := prompts.Data{"Existed": f.exists, "Path": p, "Bytes": plural(len(in.Content), "byte"), "Lines": plural(lineCount(in.Content), "line")}
	return b.result(ctx, c, OutcomeOK, prompts.Render(prompts.WriteDone, done), meta)
}

// plural renders a count with its noun, "1 line" or "2 lines", for the
// texts that name a count of lines or bytes.
func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// file is what the current-content rule found at a path.
type file struct {
	exists  bool
	content []byte
	// refusal is the result text when the rule refuses the write.
	refusal string
}

// current applies the current-content rule of spec 008 to p: a file
// that does not exist may be written, and an existing one only when its
// hash equals the last one the thread recorded for it, so a file this
// thread never read, or one changed behind its back, is refused.
func current(ctx context.Context, c Call, p string) (file, error) {
	fi, err := c.Machine.Stat(ctx, p)
	if errors.Is(err, fs.ErrNotExist) {
		return file{}, nil
	}
	if err != nil {
		return file{}, err
	}
	if fi.IsDir {
		return file{exists: true, refusal: prompts.Render(prompts.FileDirectory, prompts.Data{"Path": p})}, nil
	}
	content, err := load(ctx, c.Machine, p)
	if err != nil {
		return file{}, err
	}
	f := file{exists: true, content: content}
	switch known, ok := c.State.Hashes[p]; {
	case !ok:
		f.refusal = prompts.Render(prompts.FileUnread, prompts.Data{"Path": p})
	case known != digest(content):
		f.refusal = changedText(p)
	}
	return f, nil
}

// load reads a whole file.
func load(ctx context.Context, m machine.Machine, p string) (_ []byte, err error) {
	rc, err := m.ReadFile(ctx, p)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, rc.Close()) }()
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, rc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// lineCount is the number of lines of s, a last line without a newline
// included.
func lineCount(s string) int {
	n := strings.Count(s, "\n")
	if s != "" && !strings.HasSuffix(s, "\n") {
		n++
	}
	return n
}

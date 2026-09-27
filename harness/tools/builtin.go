// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"reflect"
	"strings"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/prompts"
)

// Names of the built-in tools, in the order of spec 008's table.
const (
	NameRead     = "read"
	NameWrite    = "write"
	NameEdit     = "edit"
	NameBash     = "bash"
	NameGrep     = "grep"
	NameGlob     = "glob"
	NameWebFetch = "web_fetch"
	NameTodo     = "todo"
)

// Builtins returns the built-in set of spec 008, in the order of its
// table: read, write, edit, bash, grep, glob, web_fetch, todo.
func Builtins() []Tool {
	return []Tool{
		readTool(), writeTool(), editTool(), bashTool(),
		grepTool(), globTool(), webFetchTool(), todoTool(),
	}
}

// builtin is one built-in tool: its definition, its properties, and the
// function that runs a call of it.
type builtin struct {
	def   Definition
	props Properties
	run   func(ctx context.Context, b *builtin, c Call) (Result, error)
}

func (b *builtin) Definition() Definition { return b.def }

func (b *builtin) Properties() Properties { return b.props }

func (b *builtin) Run(ctx context.Context, c Call) (Result, error) { return b.run(ctx, b, c) }

func newBuiltin(name, description, schema string, props Properties, run func(context.Context, *builtin, Call) (Result, error)) *builtin {
	return &builtin{
		def:   Definition{Name: name, Description: description, InputSchema: json.RawMessage(schema)},
		props: props,
		run:   run,
	}
}

// decode reads a call's input into v. The registry validated it against
// the schema, so a failure here is an input that bypassed validation,
// answered as invalid input all the same.
func (b *builtin) decode(c Call, v any) *Result {
	in := c.Input
	if len(strings.TrimSpace(string(in))) == 0 {
		in = json.RawMessage("{}")
	}
	if err := json.Unmarshal(in, v); err != nil {
		problem := "/: not valid input"
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) {
			got := te.Value
			if got == "bool" {
				got = "boolean"
			}
			problem = fmt.Sprintf("/%s: expected %s, got %s", strings.ReplaceAll(te.Field, ".", "/"), jsonKind(te.Type), got)
		}
		res := Text(OutcomeInvalidInput, invalidInput(b.def.Name, problem))
		return &res
	}
	return nil
}

// jsonKind names a Go type by the JSON type that decodes into it.
func jsonKind(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Bool:
		return "boolean"
	case reflect.Slice, reflect.Array:
		return "array"
	case reflect.String:
		return "string"
	}
	return "object"
}

// result caps text at the tool's output limit and returns the result,
// with the spill file when the text went past it.
func (b *builtin) result(ctx context.Context, c Call, outcome, text string, meta *Meta) (Result, error) {
	capped, spill, err := Cap(ctx, c.Machine, c.ID, text, b.props.OutputLimit)
	if err != nil {
		return Result{}, err
	}
	res := Text(outcome, capped)
	res.Meta, res.Spill = meta, spill
	return res, nil
}

// fail answers a machine error on p. A released machine is a failure of
// the harness and returns as a Go error; every other error is a result
// the model can act on.
func (b *builtin) fail(ctx context.Context, c Call, p string, err error) (Result, error) {
	if errors.Is(err, machine.ErrReleased) {
		return Result{}, err
	}
	return b.result(ctx, c, OutcomeError, pathError(p, err), nil)
}

// pathError is the model's sentence for a machine error on p.
func pathError(p string, err error) string {
	switch {
	case errors.Is(err, machine.ErrOutside):
		return prompts.Render(prompts.FileOutside, prompts.Data{"Path": p})
	case errors.Is(err, machine.ErrDenied):
		return prompts.Render(prompts.FileDenied, prompts.Data{"Path": p})
	case errors.Is(err, fs.ErrNotExist):
		return prompts.Render(prompts.FileNotFound, prompts.Data{"Path": p})
	case errors.Is(err, fs.ErrPermission):
		return prompts.Render(prompts.FilePermission, prompts.Data{"Path": p})
	}
	return prompts.Render(prompts.FileError, prompts.Data{"Path": p, "Error": err.Error()})
}

// resolve makes p a clean absolute machine path: a relative path resolves
// against the machine's working directory, an absolute one is kept.
func resolve(m machine.Machine, p string) string {
	if !path.IsAbs(p) {
		p = path.Join(m.Info().Workdir, p)
	}
	return path.Clean(p)
}

// digest is the hex SHA-256 of b, the hash the current-content rule
// compares.
func digest(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// changedText is the refusal of the current-content rule.
func changedText(p string) string {
	return prompts.Render(prompts.FileChanged, prompts.Data{"Path": p})
}

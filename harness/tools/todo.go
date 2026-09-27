// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	_ "embed"
	"fmt"
	"slices"
	"strings"
)

// TodoLimit is the most items a thread's list holds (spec 008).
const TodoLimit = 100

// Todo statuses.
const (
	TodoPending    = "pending"
	TodoInProgress = "in_progress"
	TodoCompleted  = "completed"
)

//go:embed descriptions/todo.md
var todoDescription string

const todoSchema = `{
  "type": "object",
  "properties": {
    "todos": {
      "type": "array",
      "description": "The whole list, which replaces the previous one; at most 100 items.",
      "items": {
        "type": "object",
        "properties": {
          "id": {"type": "string", "minLength": 1, "description": "A short id, unique in the list."},
          "content": {"type": "string", "minLength": 1, "description": "The task, in one line."},
          "status": {"type": "string", "enum": ["pending", "in_progress", "completed"]}
        },
        "required": ["id", "content", "status"],
        "additionalProperties": false
      }
    }
  },
  "required": ["todos"],
  "additionalProperties": false
}`

func todoTool() Tool {
	return newBuiltin(NameTodo, todoDescription, todoSchema, Properties{Parallel: true, Effect: EffectNone}, runTodo)
}

type todoInput struct {
	Todos []Todo `json:"todos"`
}

func runTodo(ctx context.Context, b *builtin, c Call) (Result, error) {
	var in todoInput
	if r := b.decode(c, &in); r != nil {
		return *r, nil
	}
	if len(in.Todos) > TodoLimit {
		return b.result(ctx, c, OutcomeError, fmt.Sprintf("The list has %d items; it holds at most %d.", len(in.Todos), TodoLimit), nil)
	}
	seen := make(map[string]bool, len(in.Todos))
	for i, t := range in.Todos {
		switch {
		case t.ID == "":
			return b.result(ctx, c, OutcomeError, fmt.Sprintf("Item %d has no id.", i+1), nil)
		case seen[t.ID]:
			return b.result(ctx, c, OutcomeError, fmt.Sprintf("The id %q is used twice; each item needs its own.", t.ID), nil)
		case strings.TrimSpace(t.Content) == "":
			return b.result(ctx, c, OutcomeError, fmt.Sprintf("Item %q has no content.", t.ID), nil)
		case !slices.Contains([]string{TodoPending, TodoInProgress, TodoCompleted}, t.Status):
			return b.result(ctx, c, OutcomeError, fmt.Sprintf("Item %q has the status %q; a status is pending, in_progress or completed.", t.ID, t.Status), nil)
		}
		seen[t.ID] = true
	}
	list := slices.Clone(in.Todos)
	if list == nil {
		list = []Todo{}
	}
	meta := &Meta{Todos: list}
	if len(list) == 0 {
		return b.result(ctx, c, OutcomeOK, "The todo list is empty.", meta)
	}
	done := 0
	var out strings.Builder
	for _, t := range list {
		if t.Status == TodoCompleted {
			done++
		}
	}
	fmt.Fprintf(&out, "The todo list, %d of %d completed:\n", done, len(list))
	for _, t := range list {
		fmt.Fprintf(&out, "[%s] %s: %s\n", t.Status, t.ID, t.Content)
	}
	return b.result(ctx, c, OutcomeOK, out.String(), meta)
}

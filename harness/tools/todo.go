// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"latere.ai/x/topos/prompts"
)

// TodoLimit is the most items a thread's list holds (spec 008).
const TodoLimit = 100

// Todo statuses.
const (
	TodoPending    = "pending"
	TodoInProgress = "in_progress"
	TodoCompleted  = "completed"
)

// todoSchema states the limit from TodoLimit, so the schema cannot drift
// from what runTodo enforces.
var todoSchema = fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "todos": {
      "type": "array",
      "description": "The whole list, which replaces the previous one; at most %d items.",
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
}`, TodoLimit)

func todoTool() Tool {
	return newBuiltin(NameTodo, prompts.Text(prompts.ToolTodo), todoSchema, Properties{Parallel: true, Effect: EffectNone}, runTodo)
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
		return b.result(ctx, c, OutcomeError, prompts.Render(prompts.TodoTooMany, prompts.Data{"Count": len(in.Todos), "Max": TodoLimit}), nil)
	}
	seen := make(map[string]bool, len(in.Todos))
	for i, t := range in.Todos {
		switch {
		case t.ID == "":
			return b.result(ctx, c, OutcomeError, prompts.Render(prompts.TodoNoID, prompts.Data{"Index": i + 1}), nil)
		case seen[t.ID]:
			return b.result(ctx, c, OutcomeError, prompts.Render(prompts.TodoDuplicate, prompts.Data{"ID": t.ID}), nil)
		case strings.TrimSpace(t.Content) == "":
			return b.result(ctx, c, OutcomeError, prompts.Render(prompts.TodoNoContent, prompts.Data{"ID": t.ID}), nil)
		case !slices.Contains([]string{TodoPending, TodoInProgress, TodoCompleted}, t.Status):
			return b.result(ctx, c, OutcomeError, prompts.Render(prompts.TodoBadStatus, prompts.Data{"ID": t.ID, "Status": t.Status}), nil)
		}
		seen[t.ID] = true
	}
	list := slices.Clone(in.Todos)
	if list == nil {
		list = []Todo{}
	}
	meta := &Meta{Todos: list}
	if len(list) == 0 {
		return b.result(ctx, c, OutcomeOK, prompts.Text(prompts.TodoEmpty), meta)
	}
	done := 0
	for _, t := range list {
		if t.Status == TodoCompleted {
			done++
		}
	}
	text := prompts.Render(prompts.TodoList, prompts.Data{"Done": done, "Total": len(list), "Items": list})
	return b.result(ctx, c, OutcomeOK, text, meta)
}

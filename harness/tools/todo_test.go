// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func TestTodoReplacesTheList(t *testing.T) {
	f := open(t)
	ctx := t.Context()
	todo := builtinTool(t, NameTodo)
	th := &thread{id: "thr_main"}
	list := []Todo{
		{ID: "1", Content: "read the spec", Status: TodoCompleted},
		{ID: "2", Content: "write the tool", Status: TodoInProgress},
		{ID: "3", Content: "test it", Status: TodoPending},
	}
	res := th.call(ctx, t, todo, f.h, mustInput(t, map[string]any{"todos": list}))
	want := "The todo list, 1 of 3 completed:\n[completed] 1: read the spec\n[in_progress] 2: write the tool\n[pending] 3: test it\n"
	if res.Outcome != OutcomeOK || text(res) != want {
		t.Fatalf("todo %s %q", res.Outcome, text(res))
	}
	if res.Meta == nil || !slices.Equal(res.Meta.Todos, list) || !slices.Equal(th.state().Todos, list) {
		t.Fatalf("meta %+v, state %+v", res.Meta, th.state().Todos)
	}

	// An empty list clears the thread's list, through the log.
	res = th.call(ctx, t, todo, f.h, `{"todos":[]}`)
	if res.Outcome != OutcomeOK || text(res) != "The todo list is empty." {
		t.Fatalf("clear %q", text(res))
	}
	if st := th.state(); st.Todos == nil || len(st.Todos) != 0 {
		t.Fatalf("the cleared list folds to %#v", st.Todos)
	}
}

func TestTodoValidates(t *testing.T) {
	f := open(t)
	ctx := t.Context()
	todo := builtinTool(t, NameTodo)
	many := make([]Todo, TodoLimit+1)
	for i := range many {
		many[i] = Todo{ID: fmt.Sprint(i), Content: "step", Status: TodoPending}
	}
	for _, c := range []struct {
		list []Todo
		want string
	}{
		{many, "The list has 101 items; it holds at most 100."},
		{[]Todo{{ID: "", Content: "x", Status: TodoPending}}, "Item 1 has no id."},
		{[]Todo{{ID: "a", Content: "x", Status: TodoPending}, {ID: "a", Content: "y", Status: TodoPending}}, `The id "a" is used twice; each item needs its own.`},
		{[]Todo{{ID: "a", Content: " ", Status: TodoPending}}, `Item "a" has no content.`},
		{[]Todo{{ID: "a", Content: "x", Status: "done"}}, `Item "a" has the status "done"; a status is pending, in_progress or completed.`},
	} {
		res := run(ctx, t, todo, f.h, State{}, mustInput(t, map[string]any{"todos": c.list}))
		if res.Outcome != OutcomeError || text(res) != c.want || res.Meta != nil {
			t.Fatalf("%q, want %q", text(res), c.want)
		}
	}
	if res := run(ctx, t, todo, f.h, State{}, `{}`); res.Outcome != OutcomeOK || res.Meta == nil || res.Meta.Todos == nil {
		t.Fatalf("no list at all clears it: %+v", res)
	}

	r := NewRegistry()
	if err := r.AddBuiltin(todo); err != nil {
		t.Fatal(err)
	}
	_, bad := r.Validate(NameTodo, []byte(`{"todos":[{"id":"a","content":"x","status":"done"},{"id":"b"}]}`))
	if bad == nil || bad.Outcome != OutcomeInvalidInput {
		t.Fatalf("the schema let a bad list through: %+v", bad)
	}
	for _, want := range []string{
		`/todos/0/status: must be one of "pending", "in_progress", "completed"`,
		`/todos/1: missing required property "content"`,
		`/todos/1: missing required property "status"`,
	} {
		if !strings.Contains(text(*bad), want) {
			t.Fatalf("%q lacks %q", text(*bad), want)
		}
	}
}

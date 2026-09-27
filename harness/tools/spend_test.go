// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"testing"

	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/models"
)

// spent is a machine whose core refuses every operation for spend.
type spent struct{ machine.Machine }

var errSpent = &models.SpendError{Core: machine.KindCella, Code: "budget_exhausted", Err: errors.New("the sandbox allowance is spent")}

func (spent) Exec(context.Context, machine.ExecRequest) (machine.ExecResult, error) {
	return machine.ExecResult{}, errSpent
}
func (spent) Stat(context.Context, string) (machine.FileInfo, error) {
	return machine.FileInfo{}, errSpent
}
func (spent) ReadFile(context.Context, string) (io.ReadCloser, error) { return nil, errSpent }
func (spent) WriteFile(context.Context, string, io.Reader, fs.FileMode) error {
	return errSpent
}
func (spent) Search(context.Context, machine.SearchRequest) (machine.SearchResult, error) {
	return machine.SearchResult{}, errSpent
}
func (spent) Fetch(context.Context, machine.FetchRequest) (machine.FetchResult, error) {
	return machine.FetchResult{}, errSpent
}

// TestASpendRefusalReachesTheHarness: every built-in tool returns a
// core's refusal for spend as a Go error, as it does a released machine,
// so the harness stops the turn with budget instead of the model reading
// it as a failed call.
func TestASpendRefusalReachesTheHarness(t *testing.T) {
	f := open(t)
	m := spent{f.h}
	for name, input := range map[string]string{
		NameBash:     `{"command":"ls"}`,
		NameRead:     `{"path":"a.txt"}`,
		NameWrite:    `{"path":"a.txt","content":"x"}`,
		NameEdit:     `{"path":"a.txt","old_string":"a","new_string":"b"}`,
		NameGrep:     `{"pattern":"x"}`,
		NameGlob:     `{"pattern":"*"}`,
		NameWebFetch: `{"url":"https://example.com/"}`,
	} {
		var tool Tool
		for _, b := range Builtins() {
			if b.Definition().Name == name {
				tool = b
			}
		}
		if tool == nil {
			t.Fatalf("no built-in %s", name)
		}
		_, err := tool.Run(t.Context(), Call{ID: "c1", Input: []byte(input), Machine: m})
		if code, ok := models.SpendRefused(err); !ok || code != "budget_exhausted" {
			t.Errorf("%s: %v", name, err)
		}
	}
}

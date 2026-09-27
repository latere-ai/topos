// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package tasks

import (
	"errors"
	"maps"
	"testing"
)

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestFromEnv(t *testing.T) {
	full := map[string]string{
		EnvModelsURL: "https://lux.example.com/anthropic", EnvModelsKey: "sk-test", EnvModel: "claude-sonnet-4-5",
		EnvFilter: "^instructions/", EnvRuns: "1", EnvBudget: "12.5", EnvCommit: "abc123",
	}
	o, err := FromEnv(env(full))
	if err != nil {
		t.Fatal(err)
	}
	if o.Connection.BaseURL != full[EnvModelsURL] || o.Connection.Credential != "sk-test" || o.Connection.Model != "claude-sonnet-4-5" ||
		o.Filter != "^instructions/" || o.Runs != 1 || o.BudgetUSDMicro != 12_500_000 || o.Commit != "abc123" {
		t.Fatalf("options %+v", o)
	}
	for _, missing := range []string{EnvModelsURL, EnvModelsKey, EnvModel} {
		vars := map[string]string{}
		for k, v := range full {
			if k != missing {
				vars[k] = v
			}
		}
		if _, err := FromEnv(env(vars)); !errors.Is(err, ErrNoModel) {
			t.Errorf("without %s: %v", missing, err)
		}
	}
	for k, v := range map[string]string{
		EnvModelsURL: "scripted:/tmp/s.yaml",
		EnvRuns:      "none",
		EnvBudget:    "-1",
	} {
		vars := maps.Clone(full)
		vars[k] = v
		if _, err := FromEnv(env(vars)); err == nil || errors.Is(err, ErrNoModel) {
			t.Errorf("%s=%s: %v", k, v, err)
		}
	}
	vars := map[string]string{EnvModelsURL: "ftp://lux", EnvModelsKey: "k", EnvModel: "m"}
	if _, err := FromEnv(env(vars)); err == nil || errors.Is(err, ErrNoModel) {
		t.Errorf("an ftp connection: %v", err)
	}
	vars = map[string]string{EnvModelsURL: "http://localhost:4000", EnvModelsKey: "k", EnvModel: "m"}
	if o, err := FromEnv(env(vars)); err != nil || o.Runs != 0 || o.BudgetUSDMicro != 0 {
		t.Errorf("a plain connection: %+v, %v", o, err)
	}
}

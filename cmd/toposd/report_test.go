// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// TestRunnerReportLogsEachFailure: a failure the runner reports is one
// error line naming the session and the error.
func TestRunnerReportLogsEachFailure(t *testing.T) {
	var buf bytes.Buffer
	report := runnerReport(t.Context(), slog.New(slog.NewTextHandler(&buf, nil)))
	report("ses_1", errors.New("the machine was lost"))
	if line := buf.String(); !strings.Contains(line, "level=ERROR") || !strings.Contains(line, "session=ses_1") || !strings.Contains(line, `err="the machine was lost"`) {
		t.Fatalf("the report logged %q", line)
	}
}

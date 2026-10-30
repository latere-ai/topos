// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"context"
	"log/slog"

	gotel "go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"latere.ai/x/topos/harness"
)

// Meter is the harness.Meter of hosted sessions (spec 097): two counters
// on the process's meter provider, which pkg/otel's bootstrap sets. Their
// attributes are the tool, where a call ran and why, each a closed set; the
// program a line named is in the log line alone.
var Meter harness.Meter = newMeter(gotel.Meter("latere.ai/x/topos/internal/hosted"))

type meter struct {
	runs, opens metric.Int64Counter
}

func newMeter(m metric.Meter) *meter {
	return &meter{
		runs:  counter(m, "latere.topos.bash.runs", "Bash calls of sessions with memory folders, by where they ran and why not in the process.", "{call}"),
		opens: counter(m, "latere.topos.machine.opens", "Tool calls that opened a session's machine opened on demand, by tool and why.", "{call}"),
	}
}

// counter is the named counter, or one that counts nothing when the
// provider refuses it, so a session never fails for its metrics.
func counter(m metric.Meter, name, description, unit string) metric.Int64Counter {
	c, err := m.Int64Counter(name, metric.WithDescription(description), metric.WithUnit(unit))
	if err != nil {
		slog.Warn("hosted: a counter is unavailable", "counter", name, "error", err)
		c, _ = noop.NewMeterProvider().Meter("").Int64Counter(name)
	}
	return c
}

func (m *meter) Ran(ctx context.Context, where, reason string) {
	m.runs.Add(ctx, 1, metric.WithAttributes(attribute.String("where", where), attribute.String("reason", reason)))
}

func (m *meter) Opened(ctx context.Context, tool, reason string) {
	m.opens.Add(ctx, 1, metric.WithAttributes(attribute.String("tool", tool), attribute.String("reason", reason)))
}

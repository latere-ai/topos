// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"latere.ai/x/topos/harness"
)

// TestTheMeterCountsRunsAndOpens: the hosted meter counts where bash
// calls ran and which calls opened the machine, by the closed sets of
// attributes alone (spec 097).
func TestTheMeterCountsRunsAndOpens(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	m := newMeter(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test"))
	ctx := t.Context()
	m.Ran(ctx, harness.WhereProcess, "")
	m.Ran(ctx, harness.WhereProcess, "")
	m.Ran(ctx, harness.WhereMachine, "program")
	m.Opened(ctx, "bash", "program")
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, mt := range sm.Metrics {
			sum, ok := mt.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s is %T", mt.Name, mt.Data)
			}
			for _, p := range sum.DataPoints {
				key := mt.Name
				for _, kv := range p.Attributes.ToSlice() {
					key += " " + string(kv.Key) + "=" + kv.Value.AsString()
				}
				got[key] = p.Value
			}
		}
	}
	want := map[string]int64{
		"latere.topos.bash.runs reason= where=process":        2,
		"latere.topos.bash.runs reason=program where=machine": 1,
		"latere.topos.machine.opens reason=program tool=bash": 1,
	}
	if len(got) != len(want) {
		t.Fatalf("counted %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("counted %v, want %v", got, want)
		}
	}
	if Meter == nil {
		t.Fatal("no meter for hosted sessions")
	}
}

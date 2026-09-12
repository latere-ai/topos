// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package graph_test

import (
	"encoding/json"
	"slices"
	"testing"

	"latere.ai/x/topos/graph"
)

func TestGraphPreservesEmptyToolGrants(t *testing.T) {
	for _, tools := range [][]string{nil, {}, {"read_file"}} {
		g := graph.Graph{Regions: []graph.Region{{ID: "r", Coordination: graph.Sequence, Entry: graph.Agent{Name: "a", Tools: tools}}}}
		data, err := json.Marshal(g)
		if err != nil {
			t.Fatal(err)
		}
		var back graph.Graph
		if err := json.Unmarshal(data, &back); err != nil {
			t.Fatal(err)
		}
		runtime, err := back.ToRuntime()
		if err != nil {
			t.Fatal(err)
		}
		got := runtime.Regions[0].Region.Entry.Tools
		if (got == nil) != (tools == nil) || !slices.Equal(got, tools) {
			t.Fatalf("grant %#v became %#v through %s", tools, got, data)
		}
	}
}

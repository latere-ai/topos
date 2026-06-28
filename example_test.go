// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

package topos_test

import (
	"context"
	"fmt"

	"latere.ai/x/topos"
)

// Example shows the minimal Topos Runtime loop: build a Runner, hand it a
// Region with one agent, and run a task. It uses the deterministic ModelFake
// and the default local sandbox, so it needs no API keys and no services.
func Example() {
	r, err := topos.NewRunner(topos.Options{
		SessionID: "demo",
		Model:     topos.ModelOptions{Kind: topos.ModelFake},
	})
	if err != nil {
		panic(err)
	}

	res, err := r.Run(context.Background(), topos.Region{
		Autonomy: topos.Pinned,
		Entry:    topos.AgentSpec{Name: "solo", Role: "solo", Tools: []string{"bash"}},
	}, "say hello")
	if err != nil {
		panic(err)
	}

	fmt.Println(res.Final)
	// Output: Task completed: echoed your prompt in the sandbox.
}

// ExampleRunner_Run_lineage shows that every run yields a deterministic lineage
// graph: one node per agent that ran, each with a stable id, status, granted
// tools, and the sandbox it ran in.
func ExampleRunner_Run_lineage() {
	r, _ := topos.NewRunner(topos.Options{
		SessionID: "demo",
		Model:     topos.ModelOptions{Kind: topos.ModelFake},
	})

	res, _ := r.Run(context.Background(), topos.Region{
		Autonomy: topos.Pinned,
		Entry:    topos.AgentSpec{Name: "solo", Role: "solo", Tools: []string{"bash"}},
	}, "say hello")

	for _, n := range res.Lineage.Nodes {
		fmt.Printf("%s %s\n", n.ID, n.Status)
	}
	// Output: demo/solo done
}

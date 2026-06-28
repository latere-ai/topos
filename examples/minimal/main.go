// Copyright 2026 The Latere Authors. All rights reserved.
// Use of this source code is governed by an Apache-2.0
// license that can be found in the LICENSE file.

// Command minimal is the smallest runnable Topos Runtime program: one agent,
// one task, run in-process. It uses the deterministic ModelFake and the default
// local sandbox, so it needs no API keys and no external services.
//
//	go run ./examples/minimal
package main

import (
	"context"
	"fmt"
	"log"

	"latere.ai/x/topos"
)

func main() {
	// A Runner is built from Options. ModelFake is a deterministic, network-free
	// model: turn one runs the prompt in the sandbox, turn two reports done.
	r, err := topos.NewRunner(topos.Options{
		SessionID: "minimal",
		Model:     topos.ModelOptions{Kind: topos.ModelFake},
	})
	if err != nil {
		log.Fatalf("new runner: %v", err)
	}

	// A Region is one unit of work. Pinned runs a deterministic chain; with a
	// single Entry agent that is just one step. The agent is granted the bash
	// tool so the model can run a command in its sandbox.
	region := topos.Region{
		Autonomy: topos.Pinned,
		Entry:    topos.AgentSpec{Name: "solo", Role: "solo", Tools: []string{"bash"}},
	}

	res, err := r.Run(context.Background(), region, "say hello")
	if err != nil {
		log.Fatalf("run: %v", err)
	}

	fmt.Println("final:", res.Final)

	// Every run yields a deterministic lineage graph of what ran.
	fmt.Println("lineage:")
	for _, n := range res.Lineage.Nodes {
		fmt.Printf("  %s  status=%s  sandbox=%s\n", n.ID, n.Status, n.Sandbox)
	}
}

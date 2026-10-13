// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"io"

	"latere.ai/x/topos/machine"
)

// walk lists the regular files under root, the working directory, with
// their digests (spec 073): the request is one JSON WalkRequest on
// standard input, read without waiting for the input to end, and the
// answer is one JSON response. The deny-list's paths are hidden, as they
// are from a search.
func walk(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		return fail(stderr, "walk <root>")
	}
	var q machine.WalkRequest
	if err := json.NewDecoder(stdin).Decode(&q); err != nil {
		return answer(stdout, response{Error: &errorBody{Kind: kindOther, Message: "machine: the walk request: " + err.Error()}})
	}
	r, _, err := rooted(args[0], "")
	if err != nil {
		return answer(stdout, response{Error: failure(err)})
	}
	res, err := machine.WalkFS(ctx, denyFS{FS: r.FS(), base: args[0]}, q)
	if cerr := r.Close(); err == nil && cerr != nil {
		err = cerr
	}
	if err != nil {
		return answer(stdout, response{Error: failure(err)})
	}
	return answer(stdout, response{Walk: &res})
}

// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"latere.ai/x/cella/client"

	"latere.ai/x/topos/machine"
)

// searchTimeout is Cella's bound on one search's session.
const searchTimeout = 10 * time.Minute

// Search runs grep or glob in the sandbox, where the files are: the
// helper runs machine.SearchFS over the root the path is in, with the
// deny-list's paths hidden, so a Cella machine and the host give the
// same results. The request travels as JSON on the session's input and
// the result as JSON on its output.
func (m *Machine) Search(ctx context.Context, q machine.SearchRequest) (machine.SearchResult, error) {
	p := q.Path
	if p == "" {
		p = m.Info().Workdir
	}
	t, err := m.resolve(p)
	if err != nil {
		return machine.SearchResult{}, err
	}
	q.Path = t.abs
	body, err := json.Marshal(q)
	if err != nil {
		return machine.SearchResult{}, fmt.Errorf("machine: encode the search: %w", err)
	}
	var resp response
	err = m.call(ctx, true, func(id string) error {
		sess, err := m.c.ExecSession(ctx, id, client.ExecRequest{Command: m.invoke("search", t.root), Timeout: searchTimeout.String()})
		if err != nil {
			return err
		}
		sent := make(chan error, 1)
		go func() {
			_, err := sess.Write(body)
			sent <- err
		}()
		out, rerr := readAll(sess, MaxOutput)
		code, werr := sess.Wait()
		serr := <-sent
		cerr := closeSession(sess)
		r, derr := decodeResponse(out)
		switch {
		case derr != nil && (code == 126 || code == 127):
			return fmt.Errorf("%w: exit %d: %s", errNoHelper, code, truncate(out))
		case derr != nil:
			return errors.Join(derr, rerr, werr, serr)
		}
		resp = r
		return errors.Join(rerr, werr, serr, cerr)
	})
	if err != nil {
		return machine.SearchResult{}, err
	}
	if resp.Error != nil {
		return machine.SearchResult{}, resp.Error.err("search", t.abs)
	}
	if resp.Result == nil {
		return machine.SearchResult{}, errors.New("machine: the helper answered no search result")
	}
	return *resp.Result, nil
}

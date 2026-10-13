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

// walkTimeout is Cella's bound on one walk's session. A walk hashes at
// most the bytes its request names, the copy's 256 MiB, which a
// sandbox's disk reads in seconds.
const walkTimeout = 5 * time.Minute

var _ machine.Walker = (*Machine)(nil)

// Walk lists the regular files of the sandbox's working directory with
// their digests, in the sandbox, where the files are (spec 073): the
// helper runs machine.WalkFS over the working directory with the
// deny-list's paths hidden. The request travels as JSON on the session's
// input and the result as JSON on its output, as a search's do.
func (m *Machine) Walk(ctx context.Context, q machine.WalkRequest) (machine.WalkResult, error) {
	t, err := m.resolve(m.Info().Workdir)
	if err != nil {
		return machine.WalkResult{}, err
	}
	body, err := json.Marshal(q)
	if err != nil {
		return machine.WalkResult{}, fmt.Errorf("machine: encode the walk: %w", err)
	}
	var resp response
	err = m.call(ctx, true, func(id string) error {
		sess, err := m.c.ExecSession(ctx, id, client.ExecRequest{Command: m.invoke("walk", t.abs), Timeout: walkTimeout.String()})
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
		return machine.WalkResult{}, err
	}
	if resp.Error != nil {
		return machine.WalkResult{}, resp.Error.err("walk", t.abs)
	}
	if resp.Walk == nil {
		return machine.WalkResult{}, errors.New("machine: the helper answered no walk")
	}
	return *resp.Walk, nil
}

// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package runner

import (
	"context"
	"testing"

	"latere.ai/x/topos/session"
)

// asking is a lease that asks the failover question itself, as a runner
// process's does.
type asking struct {
	session.Lease
}

func (asking) Failover(_ context.Context, failed session.ModelRef, _ string) (session.ModelRef, error) {
	return session.ModelRef{Name: "from-the-lease", Via: failed.Via}, nil
}

// TestADriveAsksTheFailoverOfItsLease: a drive's harness asks the lease's
// own failover question when the lease has one, the runner's options'
// otherwise with the session's id and the gateway's detail, and none when
// neither is set (spec 051).
func TestADriveAsksTheFailoverOfItsLease(t *testing.T) {
	failed := session.ModelRef{Name: "vendor/model-a", Via: "tier/quick"}
	var asked string
	r := &Runner{o: Options{Failover: func(_ context.Context, id string, f session.ModelRef, detail string) (session.ModelRef, error) {
		asked = id + " " + f.Name + " " + detail
		return session.ModelRef{Name: "from-the-server", Via: f.Via}, nil
	}}}
	next, err := r.failover("ses_1", nil)(t.Context(), failed, "upstream status 429")
	if err != nil || next.Name != "from-the-server" || asked != "ses_1 vendor/model-a upstream status 429" {
		t.Fatalf("the options' question answered %+v, %v, asked %q", next, err, asked)
	}
	if next, err := r.failover("ses_1", asking{})(t.Context(), failed, ""); err != nil || next.Name != "from-the-lease" {
		t.Fatalf("the lease's question answered %+v, %v", next, err)
	}
	if (&Runner{}).failover("ses_1", nil) != nil {
		t.Fatal("a runner with no question asks one")
	}
}

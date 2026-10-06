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

func (asking) Failover(_ context.Context, _, failed session.ModelRef, _, _ string) (session.ModelRef, error) {
	return session.ModelRef{Name: "from-the-lease", Via: failed.Via}, nil
}

// TestADriveAsksTheFailoverOfItsLease: a drive's harness asks the lease's
// own failover question when the lease has one, the runner's options'
// otherwise with the session's id, the model the turn stands on, the
// failed one, the reason and the detail, and none when neither is set
// (spec 051).
func TestADriveAsksTheFailoverOfItsLease(t *testing.T) {
	standing := session.ModelRef{Name: "vendor/model-a", Via: "tier/quick"}
	failed := session.ModelRef{Name: "vendor/model-b", Via: "tier/quick"}
	var asked string
	r := &Runner{o: Options{Failover: func(_ context.Context, id string, on, f session.ModelRef, reason, detail string) (session.ModelRef, error) {
		asked = id + " " + on.Name + " " + f.Name + " " + reason + " " + detail
		return session.ModelRef{Name: "from-the-server", Via: f.Via}, nil
	}}}
	next, err := r.failover("ses_1", nil)(t.Context(), standing, failed, "rejected", "upstream_rejected: upstream status 404")
	if err != nil || next.Name != "from-the-server" || asked != "ses_1 vendor/model-a vendor/model-b rejected upstream_rejected: upstream status 404" {
		t.Fatalf("the options' question answered %+v, %v, asked %q", next, err, asked)
	}
	if next, err := r.failover("ses_1", asking{})(t.Context(), standing, failed, "", ""); err != nil || next.Name != "from-the-lease" {
		t.Fatalf("the lease's question answered %+v, %v", next, err)
	}
	if (&Runner{}).failover("ses_1", nil) != nil {
		t.Fatal("a runner with no question asks one")
	}
}

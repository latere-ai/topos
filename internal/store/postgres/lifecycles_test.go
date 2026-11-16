// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

//go:build postgres

package postgres

import (
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"latere.ai/x/pkg/statemachine"
	"latere.ai/x/pkg/statemachine/statemachinetest"

	"latere.ai/x/topos/internal/store"
	objstoretest "latere.ai/x/topos/internal/store/storetest"
	"latere.ai/x/topos/session"
	sessionstoretest "latere.ai/x/topos/session/storetest"
)

// The writers of every lifecycle the store keeps are driven through its
// table on Postgres: Exhaustive asks each writer for every state from every
// state, and Race starts two writers at once on a record in each state.

var lifecycleSeq atomic.Int64

// refused is what a writer's answer of "nothing to do" becomes.
func refused(what string) error {
	return fmt.Errorf("%s: no change: %w", what, statemachine.ErrIllegalTransition)
}

// noWriter is the refusal for a state no writer moves a record to.
func noWriter(what string) error {
	return fmt.Errorf("%s: no writer takes a record there: %w", what, statemachine.ErrIllegalTransition)
}

func TestAgentWriters(t *testing.T) {
	st := fresh(t, Options{})
	ctx := t.Context()
	d := statemachinetest.Driver[store.AgentState]{
		Enter: func(t *testing.T, state store.AgentState) string {
			id := session.NewID(session.PrefixAgent)
			a := store.Agent{ID: id, Name: fmt.Sprintf("agent-%d", lifecycleSeq.Add(1)), Owner: "alice", CreatedAt: time.Now().UTC()}
			v := store.AgentVersion{AgentID: id, Version: 1, Digest: "sha256:1", Doc: []byte("{}"), Bundle: []byte("{}"), CreatedBy: "alice", CreatedAt: a.CreatedAt}
			if err := st.PutVersion(ctx, a, v); err != nil {
				t.Fatalf("create: %v", err)
			}
			switch state {
			case store.AgentArchived:
				if err := st.Archive(ctx, id, time.Now()); err != nil {
					t.Fatalf("archive: %v", err)
				}
			case store.AgentDeleted:
				if err := st.DeleteAgent(ctx, id); err != nil {
					t.Fatalf("delete: %v", err)
				}
			case store.AgentActive:
			}
			return id
		},
		Move: func(id string, to store.AgentState) error {
			var err error
			switch to {
			case store.AgentArchived:
				err = st.Archive(ctx, id, time.Now())
			case store.AgentDeleted:
				err = st.DeleteAgent(ctx, id)
			case store.AgentActive:
				return noWriter("agent active")
			}
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("agent %s is deleted: %w", id, statemachine.ErrIllegalTransition)
			}
			return err
		},
		State: func(t *testing.T, id string) store.AgentState {
			a, err := st.Agent(ctx, id)
			switch {
			case errors.Is(err, store.ErrNotFound):
				return store.AgentDeleted
			case err != nil:
				t.Fatalf("read the agent: %v", err)
			case a.ArchivedAt != nil:
				return store.AgentArchived
			}
			return store.AgentActive
		},
	}
	statemachinetest.Exhaustive(t, store.AgentTable, d)
	statemachinetest.Race(t, store.AgentTable, d)
}

func TestIdempotencyWriters(t *testing.T) {
	clock := objstoretest.NewClock()
	st := fresh(t, Options{Now: clock.Now})
	ctx := t.Context()
	record := func(key string) store.Idempotency {
		return store.Idempotency{Subject: "alice", Key: key, Route: "POST /v1/sessions", BodyHash: "h", Status: 201, ExpiresAt: clock.Now().Add(time.Hour)}
	}
	d := statemachinetest.Driver[store.IdempotencyState]{
		Enter: func(t *testing.T, state store.IdempotencyState) string {
			key := fmt.Sprintf("k%d", lifecycleSeq.Add(1))
			if _, fresh, err := st.Begin(ctx, record(key)); err != nil || !fresh {
				t.Fatalf("reserve: %v, fresh %v", err, fresh)
			}
			switch state {
			case store.IdempotencyDone:
				if err := st.Finish(ctx, record(key)); err != nil {
					t.Fatalf("finish: %v", err)
				}
			case store.IdempotencyRemoved:
				if err := st.Abandon(ctx, "alice", key); err != nil {
					t.Fatalf("abandon: %v", err)
				}
			case store.IdempotencyReserved:
			}
			return key
		},
		Move: func(key string, to store.IdempotencyState) error {
			switch to {
			case store.IdempotencyDone:
				err := st.Finish(ctx, record(key))
				if errors.Is(err, store.ErrNotFound) {
					return fmt.Errorf("record %s is removed: %w", key, statemachine.ErrIllegalTransition)
				}
				return err
			case store.IdempotencyRemoved:
				removed, err := st.abandon(ctx, "alice", key)
				if err == nil && !removed {
					return refused("abandon " + key)
				}
				return err
			case store.IdempotencyReserved:
				return noWriter("idempotency reserved")
			}
			return noWriter("idempotency " + string(to))
		},
		State: func(t *testing.T, key string) store.IdempotencyState {
			var done bool
			err := st.pool.QueryRow(ctx, `SELECT done FROM idempotency_keys WHERE subject = 'alice' AND key = $1`, key).Scan(&done)
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				return store.IdempotencyRemoved
			case err != nil:
				t.Fatalf("read the record: %v", err)
			case done:
				return store.IdempotencyDone
			}
			return store.IdempotencyReserved
		},
	}
	statemachinetest.Exhaustive(t, store.IdempotencyTable, d)
	statemachinetest.Race(t, store.IdempotencyTable, d)
}

func TestFiringWriters(t *testing.T) {
	clock := objstoretest.NewClock()
	st := fresh(t, Options{Now: clock.Now})
	ctx := t.Context()
	type key struct{ trigger, dedupe, id string }
	keys := map[string]key{}
	d := statemachinetest.Driver[store.FiringState]{
		Enter: func(t *testing.T, state store.FiringState) string {
			tr := objstoretest.NewTrigger("alice", fmt.Sprintf("trigger-%d", lifecycleSeq.Add(1)), clock.Now())
			if err := st.PutTrigger(ctx, tr); err != nil {
				t.Fatalf("create the trigger: %v", err)
			}
			f := objstoretest.NewFiring(tr.ID, "d1", store.OriginEvent, clock.Now())
			got, fresh, err := st.ClaimFiring(ctx, f)
			if err != nil || !fresh {
				t.Fatalf("claim: %v, fresh %v", err, fresh)
			}
			k := key{tr.ID, "d1", got.ID}
			keys[got.ID] = k
			if state != store.FiringClaimed {
				rec := got
				rec.Outcome = state.Outcome()
				if err := st.RecordFiring(ctx, rec, ""); err != nil {
					t.Fatalf("record %s: %v", state, err)
				}
			}
			return got.ID
		},
		Move: func(id string, to store.FiringState) error {
			k := keys[id]
			var outcome string
			if err := st.pool.QueryRow(ctx, `SELECT outcome FROM trigger_firings WHERE trigger_id = $1 AND dedupe = $2`, k.trigger, k.dedupe).Scan(&outcome); err != nil {
				return err
			}
			stored := objstoretest.NewFiring(k.trigger, k.dedupe, store.OriginEvent, clock.Now())
			stored.ID = id
			if to == store.FiringClaimed {
				claim := stored
				claim.RetakeRefused = true
				_, _, retaken, err := st.claimFiring(ctx, claim)
				if err == nil && !retaken {
					return refused("claim " + id)
				}
				return err
			}
			stored.Outcome = to.Outcome()
			err := st.RecordFiring(ctx, stored, outcome)
			if errors.Is(err, store.ErrConflict) {
				return errors.Join(err, statemachine.ErrIllegalTransition)
			}
			return err
		},
		State: func(t *testing.T, id string) store.FiringState {
			k := keys[id]
			var outcome string
			if err := st.pool.QueryRow(ctx, `SELECT outcome FROM trigger_firings WHERE trigger_id = $1 AND dedupe = $2`, k.trigger, k.dedupe).Scan(&outcome); err != nil {
				t.Fatalf("read the firing: %v", err)
			}
			state, ok := store.FiringStateOf(outcome)
			if !ok {
				t.Fatalf("outcome %q is no state", outcome)
			}
			return state
		},
	}
	statemachinetest.Exhaustive(t, store.FiringTable, d)
	statemachinetest.Race(t, store.FiringTable, d)
}

func TestRunWriters(t *testing.T) {
	clock := objstoretest.NewClock()
	st := fresh(t, Options{Now: clock.Now})
	ctx := t.Context()
	d := statemachinetest.Driver[store.RunState]{
		Enter: func(t *testing.T, state store.RunState) string {
			r := store.Run{
				FiringID: session.NewID(session.PrefixFiring), Owner: "alice", TriggerID: session.NewID(session.PrefixTrigger),
				SessionID: "ses_1", At: clock.Now(), Outcome: store.OutcomeStarted, Ended: state == store.RunEnded,
			}
			if err := st.PutRun(ctx, r); err != nil {
				t.Fatalf("put the run: %v", err)
			}
			return r.FiringID
		},
		Move: func(id string, to store.RunState) error {
			switch to {
			case store.RunEnded:
				n, err := st.setRunCosts(ctx, []store.RunCost{{FiringID: id, CostUSDMicro: 10, Ended: true}})
				if err == nil && n == 0 {
					return refused("end the run " + id)
				}
				return err
			case store.RunOpen:
				return noWriter("run open")
			}
			return noWriter("run " + string(to))
		},
		State: func(t *testing.T, id string) store.RunState {
			var ended bool
			if err := st.pool.QueryRow(ctx, `SELECT ended FROM trigger_runs WHERE firing_id = $1`, id).Scan(&ended); err != nil {
				t.Fatalf("read the run: %v", err)
			}
			if ended {
				return store.RunEnded
			}
			return store.RunOpen
		},
	}
	statemachinetest.Exhaustive(t, store.RunTable, d)
	statemachinetest.Race(t, store.RunTable, d)
}

func TestArchiveWriters(t *testing.T) {
	st := fresh(t, Options{})
	ctx := t.Context()
	d := statemachinetest.Driver[session.ArchiveState]{
		Enter: func(t *testing.T, state session.ArchiveState) string {
			s := sessionstoretest.NewSession()
			if err := st.Create(ctx, s, nil); err != nil {
				t.Fatalf("create: %v", err)
			}
			if state == session.ArchiveArchived {
				at := time.Now()
				if _, err := st.SetArchived(ctx, s.ID, &at); err != nil {
					t.Fatalf("archive: %v", err)
				}
			}
			return s.ID
		},
		Move: func(id string, to session.ArchiveState) error {
			var at *time.Time
			if to == session.ArchiveArchived {
				now := time.Now()
				at = &now
			}
			_, changed, err := st.setArchived(ctx, id, at)
			if err == nil && !changed {
				return refused("archive " + id)
			}
			return err
		},
		State: func(t *testing.T, id string) session.ArchiveState {
			s, err := st.Get(ctx, id)
			if err != nil {
				t.Fatalf("read the session: %v", err)
			}
			if s.ArchivedAt != nil {
				return session.ArchiveArchived
			}
			return session.ArchiveActive
		},
	}
	statemachinetest.Exhaustive(t, session.ArchiveTable, d)
	statemachinetest.Race(t, session.ArchiveTable, d)
}

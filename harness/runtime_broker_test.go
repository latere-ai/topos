package harness_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"latere.ai/x/agents/internal/harness"
	fakemodelimpl "latere.ai/x/agents/internal/models/fake"
	"latere.ai/x/agents/internal/sandbox/local"
	"latere.ai/x/agents/internal/store"
	"latere.ai/x/agents/internal/store/memory"
	"latere.ai/x/agents/internal/trustplane"
)

// TestRuntimeRunBrokersSessionCredential asserts that a Runtime with a broker
// mints a model-channel key at run start and revokes it on SessionEnd, leaving
// a mint+revoke audit trail — the credential lifecycle crash recovery and
// budget enforcement build on.
func TestRuntimeRunBrokersSessionCredential(t *testing.T) {
	var mintCalls, revokeCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /lux/v1/keys", func(w http.ResponseWriter, _ *http.Request) {
		mintCalls.Add(1)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "vk_run", "token": "lux_run"})
	})
	mux.HandleFunc("DELETE /lux/v1/keys/{id}", func(w http.ResponseWriter, _ *http.Request) {
		revokeCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	log := trustplane.NewMemoryLog()
	broker := trustplane.NewBroker(trustplane.Config{
		Lux:         trustplane.NewLuxClient(srv.URL, "bearer", nil),
		Audit:       log,
		Bindings:    map[string]string{"anthropic": "pk_1"},
		SpendCapUSD: 5,
		SessionTTL:  time.Hour,
	})

	rt := harness.New(fakemodelimpl.New(), local.New(), nil, nil).WithBroker(broker)
	_, err := rt.Run(context.Background(), &store.Agent{ID: "a", DisplayName: "x", Kind: "worker"}, "do it")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if mintCalls.Load() != 1 {
		t.Fatalf("mint calls = %d, want 1", mintCalls.Load())
	}
	if revokeCalls.Load() != 1 {
		t.Fatalf("revoke calls = %d, want 1 (revoked on SessionEnd)", revokeCalls.Load())
	}

	var mint, revoke int
	for _, e := range log.Entries() {
		switch e.Action {
		case trustplane.ActionMint:
			mint++
		case trustplane.ActionRevoke:
			revoke++
		}
	}
	if mint != 1 || revoke != 1 {
		t.Fatalf("audit: mint=%d revoke=%d, want 1/1", mint, revoke)
	}
}

// TestRuntimeRunStoreAndBrokerCompose asserts the two opt-ins coexist on one
// run: the session is persisted + marked completed AND the credential is minted
// then revoked — the composition no per-spec test exercises on its own.
func TestRuntimeRunStoreAndBrokerCompose(t *testing.T) {
	var mintCalls, revokeCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /lux/v1/keys", func(w http.ResponseWriter, _ *http.Request) {
		mintCalls.Add(1)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "vk_c", "token": "lux_c"})
	})
	mux.HandleFunc("DELETE /lux/v1/keys/{id}", func(w http.ResponseWriter, _ *http.Request) {
		revokeCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	sessions := memory.NewSessionStore()
	log := trustplane.NewMemoryLog()
	broker := trustplane.NewBroker(trustplane.Config{
		Lux:   trustplane.NewLuxClient(srv.URL, "bearer", nil),
		Audit: log,
	})
	rt := harness.New(fakemodelimpl.New(), local.New(), nil, nil).
		WithSessionStore(sessions).
		WithBroker(broker)

	result, err := rt.Run(context.Background(), &store.Agent{ID: "a", DisplayName: "x", Kind: "worker"}, "go")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Session store side.
	sess, err := sessions.GetSession(context.Background(), result.SessionID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if sess.Status != store.SessionCompleted {
		t.Fatalf("status = %q, want completed", sess.Status)
	}
	// Broker side.
	if mintCalls.Load() != 1 || revokeCalls.Load() != 1 {
		t.Fatalf("mint=%d revoke=%d, want 1/1", mintCalls.Load(), revokeCalls.Load())
	}
}

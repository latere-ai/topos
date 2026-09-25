// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package cella_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1 "latere.ai/x/cella/manifest/v1"

	"latere.ai/x/topos/sandbox"
	"latere.ai/x/topos/sandbox/cella"
)

// TestCreateHoldsUntilRunning is the create contract: a v1beta1 manifest with
// the base image, the agent label, the idle stop and the ephemeral time to
// live, an explicit allowlist for the model gateway's origin, and a create held
// with ?wait=1 so the sandbox is running when Create returns.
func TestCreateHoldsUntilRunning(t *testing.T) {
	f := newFakeCore(t)
	sb, err := f.provider(t).Create(t.Context(), sandbox.CreateOptions{
		Name:   "agent-1",
		Labels: map[string]string{"team": "core"},
		Env:    map[string]string{"MODEL": "small"},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	want := sandbox.Sandbox{ID: "sbx_1", Name: "agent-1", State: sandbox.StateRunning, Tier: "ephemeral", CreatedAt: "2026-09-26T10:00:00Z"}
	if sb != want {
		t.Errorf("Create = %+v, want %+v", sb, want)
	}
	m := f.lastManifest(t)
	if m.APIVersion != "cella.latere.ai/v1beta1" || m.Kind != "Sandbox" {
		t.Errorf("envelope = %s %s, want cella.latere.ai/v1beta1 Sandbox", m.APIVersion, m.Kind)
	}
	if m.Metadata.Name != "agent-1" || m.Metadata.Labels["kind"] != "agent" || m.Metadata.Labels["team"] != "core" {
		t.Errorf("metadata = %+v", m.Metadata)
	}
	if m.Spec.Image != "base" || m.Spec.Env["MODEL"] != "small" {
		t.Errorf("spec image/env = %q %v", m.Spec.Image, m.Spec.Env)
	}
	if e := m.Spec.Network.Egress; e.Mode != v1.EgressAllowlist || !slices.Equal(e.AllowedHosts, []string{"api.latere.ai"}) || e.DeniedHosts != nil {
		t.Errorf("egress = %+v, want allowlist of api.latere.ai", e)
	}
	if l := m.Spec.Lifecycle; l.AutoStop != "15m" || l.TTL != "24h" || l.AutoDelete != "" {
		t.Errorf("lifecycle = %+v, want autoStop 15m and ttl 24h", l)
	}
	q, err := url.ParseQuery(f.createQs[0])
	if err != nil || q.Get("wait") != "1" || q.Get("timeout") != "10m0s" {
		t.Errorf("create query = %q, want wait=1 and the ten-minute hold", f.createQs[0])
	}
}

// TestCreatePendingThenRunningOnRead covers a hold that ends before the
// sandbox runs: Create returns it creating, and a read once the scheduler has
// moved it reports it running.
func TestCreatePendingThenRunningOnRead(t *testing.T) {
	f := newFakeCore(t)
	f.holdPhase = "Pending"
	p := f.provider(t)
	sb, err := p.Create(t.Context(), sandbox.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sb.State != sandbox.StateCreating || sb.Name != "sandbox-1" {
		t.Fatalf("Create = %+v, want a creating sandbox the server named", sb)
	}
	if err := p.HealthCheck(t.Context(), sb.ID); err != nil {
		t.Fatalf("HealthCheck after the scheduler ran: %v", err)
	}
}

// TestCreateHoldBoundedByDeadline asks the server to answer before the caller
// gives up, so the answer names the sandbox it made.
func TestCreateHoldBoundedByDeadline(t *testing.T) {
	f := newFakeCore(t)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	if _, err := f.provider(t).Create(ctx, sandbox.CreateOptions{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	q, _ := url.ParseQuery(f.createQs[0])
	hold, err := time.ParseDuration(q.Get("timeout"))
	if err != nil || hold > 25*time.Second || hold < 20*time.Second {
		t.Errorf("hold = %q, want the remaining 30s less the margin", q.Get("timeout"))
	}
}

func TestCreatePersistentTierAndCatalogImage(t *testing.T) {
	f := newFakeCore(t)
	sb, err := f.provider(t).Create(t.Context(), sandbox.CreateOptions{Tier: "persistent", Image: "gui"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	m := f.lastManifest(t)
	if m.Spec.Image != "gui" {
		t.Errorf("image = %q, want gui", m.Spec.Image)
	}
	if l := m.Spec.Lifecycle; l.AutoStop != "15m" || l.TTL != "" || l.AutoDelete != "" {
		t.Errorf("lifecycle = %+v, want the idle stop alone", l)
	}
	if sb.Tier != "persistent" {
		t.Errorf("tier = %q, want persistent", sb.Tier)
	}
}

// TestCreateTierReadFromObject holds that the tier is what the object does: an
// admission that gives a persistent request a time to live makes it ephemeral.
func TestCreateTierReadFromObject(t *testing.T) {
	f := newFakeCore(t)
	f.admitTTL = "72h"
	sb, err := f.provider(t).Create(t.Context(), sandbox.CreateOptions{Tier: "persistent"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if sb.Tier != "ephemeral" {
		t.Errorf("tier = %q, want ephemeral for a sandbox the plane deletes after 72h", sb.Tier)
	}
	f.admitTTL = v1.DurationNever
	sb, err = f.provider(t).Create(t.Context(), sandbox.CreateOptions{Tier: "persistent"})
	if err != nil || sb.Tier != "persistent" {
		t.Errorf("Create with ttl never = %+v, %v, want persistent", sb, err)
	}
}

// TestCreateRefusesWhatTheCoreDoesNotCarry holds the refusals: each field asks
// for a restriction or a credential, and nothing is sent for any of them.
func TestCreateRefusesWhatTheCoreDoesNotCarry(t *testing.T) {
	for name, opts := range map[string]sandbox.CreateOptions{
		"policy":        {Policy: "spawner"},
		"secret mounts": {SecretMounts: []string{"OPENAI_API_KEY"}},
		"unknown tier":  {Tier: "forever"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeCore(t)
			_, err := f.provider(t).Create(t.Context(), opts)
			if err == nil {
				t.Fatal("Create succeeded, want a refusal")
			}
			if got := f.requestLog(); len(got) != 0 {
				t.Errorf("requests = %v, want none", got)
			}
		})
	}
}

// TestCreateEmptySecretMountsMountsNothing keeps the nil and empty cases
// working: both mount nothing, which the control plane needs no field for.
func TestCreateEmptySecretMountsMountsNothing(t *testing.T) {
	f := newFakeCore(t)
	if _, err := f.provider(t).Create(t.Context(), sandbox.CreateOptions{SecretMounts: []string{}}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s := f.lastManifest(t).Spec.Secrets; s != nil {
		t.Errorf("secrets = %v, want none", s)
	}
}

func TestCreateAllowedHostsReplacesTheDefault(t *testing.T) {
	f := newFakeCore(t)
	hosts := []string{"api.latere.ai", "*.github.com"}
	p := cella.New(cella.Options{BaseURL: f.url(), Token: cella.StaticTokenSource("t"), HTTPClient: f.srv.Client(), AllowedHosts: hosts})
	hosts[1] = "changed.example" // the provider holds its own copy
	if _, err := p.Create(t.Context(), sandbox.CreateOptions{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if e := f.lastManifest(t).Spec.Network.Egress; e.Mode != v1.EgressAllowlist || !slices.Equal(e.AllowedHosts, []string{"api.latere.ai", "*.github.com"}) {
		t.Errorf("egress = %+v", e)
	}
}

// TestCreateFailedIsDeleted: a sandbox the control plane left Failed is
// reported by id and reason and removed, since the caller gets no handle to it.
func TestCreateFailedIsDeleted(t *testing.T) {
	f := newFakeCore(t)
	f.holdPhase, f.holdReason = "Failed", "CreateFailed"
	_, err := f.provider(t).Create(t.Context(), sandbox.CreateOptions{})
	if err == nil || !strings.Contains(err.Error(), "sbx_1") || !strings.Contains(err.Error(), "CreateFailed") {
		t.Fatalf("Create = %v, want an error naming sbx_1 and CreateFailed", err)
	}
	if _, ok := f.lookup("sbx_1"); ok {
		t.Error("the failed sandbox was not deleted")
	}
	if got := f.requestLog(); !slices.Equal(got, []string{"POST " + basePath + "/sandboxes", "DELETE " + basePath + "/sandboxes/sbx_1"}) {
		t.Errorf("requests = %v", got)
	}
}

func TestCreateFailedDeleteFailureIsReported(t *testing.T) {
	f := newFakeCore(t)
	f.holdPhase, f.holdReason = "Lost", "NodeGone"
	f.deleteErr = &reply{http.StatusServiceUnavailable, "driver_unavailable", "The runtime is not answering."}
	_, err := f.provider(t).Create(t.Context(), sandbox.CreateOptions{})
	var apiErr *sandbox.APIError
	if err == nil || !strings.Contains(err.Error(), "NodeGone") || !errors.As(err, &apiErr) || apiErr.Code != "driver_unavailable" {
		t.Fatalf("Create = %v, want the start failure joined with the delete refusal", err)
	}
}

// TestCreateEgressGatewayUnavailable: a control plane with no egress gateway
// refuses the allowlist boundary, and the refusal reaches the caller whole.
func TestCreateEgressGatewayUnavailable(t *testing.T) {
	f := newFakeCore(t)
	f.createErr = &reply{http.StatusServiceUnavailable, "egress_gateway_unavailable", "No egress gateway is connected."}
	_, err := f.provider(t).Create(t.Context(), sandbox.CreateOptions{})
	var apiErr *sandbox.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("Create = %v, want *sandbox.APIError", err)
	}
	if apiErr.Status != http.StatusServiceUnavailable || apiErr.Code != "egress_gateway_unavailable" || apiErr.RequestID != "req_fake" || apiErr.Message == "" {
		t.Errorf("APIError = %+v", apiErr)
	}
}

func TestCreateConflict(t *testing.T) {
	f := newFakeCore(t)
	f.createErr = &reply{http.StatusConflict, "already_exists", "The name is taken."}
	if _, err := f.provider(t).Create(t.Context(), sandbox.CreateOptions{Name: "taken"}); !errors.Is(err, sandbox.ErrConflict) {
		t.Fatalf("Create = %v, want ErrConflict", err)
	}
}

func TestDestroyIsIdempotent(t *testing.T) {
	f := newFakeCore(t)
	p := f.provider(t)
	sb, err := p.Create(t.Context(), sandbox.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := p.Destroy(t.Context(), sb.ID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if _, ok := f.lookup(sb.ID); ok {
		t.Error("the sandbox is still there")
	}
	if err := p.Destroy(t.Context(), sb.ID); err != nil {
		t.Errorf("second Destroy = %v, want nil for a sandbox already gone", err)
	}
	f.deleteErr = &reply{http.StatusInternalServerError, "internal", "boom"}
	var apiErr *sandbox.APIError
	if err := p.Destroy(t.Context(), "sbx_9"); !errors.As(err, &apiErr) || apiErr.Status != http.StatusInternalServerError {
		t.Errorf("Destroy on a refusal = %v, want *sandbox.APIError", err)
	}
}

func TestHealthCheck(t *testing.T) {
	f := newFakeCore(t)
	p := f.provider(t)
	sb, err := p.Create(t.Context(), sandbox.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := p.HealthCheck(t.Context(), sb.ID); err != nil {
		t.Errorf("HealthCheck running = %v", err)
	}
	f.readPhase = "Stopped"
	if err := p.HealthCheck(t.Context(), sb.ID); err == nil || !strings.Contains(err.Error(), "phase=Stopped") {
		t.Errorf("HealthCheck stopped = %v, want the phase named", err)
	}
	if err := p.HealthCheck(t.Context(), "sbx_missing"); !errors.Is(err, sandbox.ErrNotFound) {
		t.Errorf("HealthCheck missing = %v, want ErrNotFound", err)
	}
}

// TestHealthCheckNamesTheReason: the reason a sandbox is not running is part
// of the error a caller logs.
func TestHealthCheckNamesTheReason(t *testing.T) {
	f := newFakeCore(t)
	p := f.provider(t)
	sb, err := p.Create(t.Context(), sandbox.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.readPhase, f.readReason = "Stopped", "AutoStop"
	if err := p.HealthCheck(t.Context(), sb.ID); err == nil || !strings.Contains(err.Error(), "phase=Stopped reason=AutoStop") {
		t.Errorf("HealthCheck = %v, want the phase and reason named", err)
	}
}

// TestTokenAskedPerRequest: the provider holds no token, so a renewed one is
// presented on the next request.
func TestTokenAskedPerRequest(t *testing.T) {
	f := newFakeCore(t)
	var n atomic.Int32
	p := cella.New(cella.Options{
		BaseURL:    f.url(),
		HTTPClient: f.srv.Client(),
		Token: cella.TokenFunc(func(context.Context) (string, error) {
			return "tok-" + string(rune('a'+n.Add(1)-1)), nil
		}),
	})
	sb, err := p.Create(t.Context(), sandbox.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := p.HealthCheck(t.Context(), sb.ID); err != nil {
		t.Fatalf("HealthCheck: %v", err)
	}
	f.mu.Lock()
	got := append([]string(nil), f.bearers...)
	f.mu.Unlock()
	if !slices.Equal(got, []string{"tok-a", "tok-b"}) {
		t.Errorf("bearers = %v, want a fresh token per request", got)
	}
}

func TestTokenErrorSendsNothing(t *testing.T) {
	f := newFakeCore(t)
	boom := errors.New("token unavailable")
	p := cella.New(cella.Options{BaseURL: f.url(), HTTPClient: f.srv.Client(),
		Token: cella.TokenFunc(func(context.Context) (string, error) { return "", boom })})
	if _, err := p.Create(t.Context(), sandbox.CreateOptions{}); !errors.Is(err, boom) {
		t.Fatalf("Create = %v, want the token error", err)
	}
	if got := f.requestLog(); len(got) != 0 {
		t.Errorf("requests = %v, want none", got)
	}
}

func TestContextBearerThreadsThrough(t *testing.T) {
	f := newFakeCore(t)
	p := cella.New(cella.Options{BaseURL: f.url(), HTTPClient: f.srv.Client(), Token: cella.ContextTokenSource{}})
	if _, err := p.Create(sandbox.WithBearer(t.Context(), "user-token"), sandbox.CreateOptions{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if f.bearers[0] != "user-token" {
		t.Errorf("bearer = %q, want the context's", f.bearers[0])
	}
	if _, err := p.Create(t.Context(), sandbox.CreateOptions{}); err == nil {
		t.Error("Create without a bearer succeeded")
	}
}

// countingTransport counts the requests that pass through it.
type countingTransport struct {
	n    atomic.Int32
	base http.RoundTripper
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return c.base.RoundTrip(r)
}

// TestRequestsUseTheConfiguredClient guards the one deadline the exported
// client's own transport would impose: ten seconds to the first byte, which a
// held create and a long command exceed. Every request must go through the
// client the provider was given.
func TestRequestsUseTheConfiguredClient(t *testing.T) {
	f := newFakeCore(t)
	counter := &countingTransport{base: f.srv.Client().Transport}
	p := cella.New(cella.Options{BaseURL: f.url(), Token: cella.StaticTokenSource("t"), HTTPClient: &http.Client{Transport: counter}})
	sb, err := p.Create(t.Context(), sandbox.CreateOptions{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := p.Exec(t.Context(), sb.ID, sandbox.ExecOptions{Argv: []string{"true"}}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if err := p.WriteFile(t.Context(), sb.ID, "a.txt", []byte("x")); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := p.Destroy(t.Context(), sb.ID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if got, want := int(counter.n.Load()), len(f.requestLog()); got != want || got != 4 {
		t.Errorf("requests through the configured client = %d, the fake saw %d, want 4", got, want)
	}
}

// TestDefaultClientReachesTheCore: with no HTTPClient the provider still has
// one of its own and reaches the control plane.
func TestDefaultClientReachesTheCore(t *testing.T) {
	f := newFakeCore(t)
	p := cella.New(cella.Options{BaseURL: f.url(), Token: cella.StaticTokenSource("t")})
	if _, err := p.Create(t.Context(), sandbox.CreateOptions{}); err != nil {
		t.Fatalf("Create: %v", err)
	}
}

func TestUnreachableIsNotAnAPIError(t *testing.T) {
	f := newFakeCore(t)
	p := f.provider(t)
	f.srv.Close()
	_, err := p.Create(t.Context(), sandbox.CreateOptions{})
	var apiErr *sandbox.APIError
	if err == nil || errors.As(err, &apiErr) {
		t.Fatalf("Create against a closed server = %v, want a transport error", err)
	}
}

func TestNewPanicsOnMissingConfig(t *testing.T) {
	for name, opts := range map[string]cella.Options{
		"no base url": {Token: cella.StaticTokenSource("t")},
		"no token":    {BaseURL: "https://api.latere.ai/v1/environments"},
		"bad scheme":  {BaseURL: "ftp://api.latere.ai", Token: cella.StaticTokenSource("t")},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("New did not panic")
				}
			}()
			cella.New(opts)
		})
	}
}

func TestStaticTokenSource(t *testing.T) {
	if tok, err := cella.StaticTokenSource("abc").Token(t.Context()); err != nil || tok != "abc" {
		t.Errorf("Token = %q, %v", tok, err)
	}
	if _, err := cella.StaticTokenSource("").Token(t.Context()); err == nil {
		t.Error("an empty static token was accepted")
	}
}

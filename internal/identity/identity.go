// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package identity is toposd's client of the identity provider that
// hosts the installation's agents (spec 018): toposd authenticates as
// its host client with the client_credentials grant, creates an agent's
// identity when the agent is first applied, archives it with the agent,
// disables it once the archived agent has no session left, lists the
// archived ones to reconcile, and asks for a token for one agent, one
// audience and one session. An agent holds no key; the host's one
// credential is the only secret here, and it is read from a file at each
// fetch so it rotates in place.
package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"latere.ai/x/pkg/authkit/oidc"
)

// The owner types an identity is recorded with.
const (
	OwnerUser         = "user"
	OwnerOrganization = "organization"
)

// The workloads a session claim names: the runner's own calls, or the
// session's sandbox.
const (
	WorkloadSession = "session"
	WorkloadSandbox = "sandbox"
)

// hostTokenMargin is how long before its expiry the host's own token is
// fetched again.
const hostTokenMargin = 2 * time.Minute

// maxLifetime is the longest a hosted-agent token lives.
const maxLifetime = 15 * time.Minute

// ErrUnavailable is an identity provider that did not answer, or
// answered with a server error: the call may be retried.
var ErrUnavailable = errors.New("identity: the identity provider did not answer")

// Error is a refusal the identity provider answered, with its code.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("identity: the identity provider refused with %d %s: %s", e.Status, e.Code, e.Message)
}

// Owner is the person or organization an agent belongs to.
type Owner struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// OwnerOf is the owner of an agent applied with a token of sub whose
// claims are claims: the organization its org_id claim names, which the
// identity provider sets for a token in an organization's context, or
// else the person.
func OwnerOf(sub string, claims map[string]any) Owner {
	if org, _ := claims["org_id"].(string); org != "" {
		return Owner{Type: OwnerOrganization, ID: org}
	}
	return Owner{Type: OwnerUser, ID: sub}
}

// Session is the session claim of a hosted-agent token.
type Session struct {
	ID       string `json:"id"`
	Workload string `json:"workload"`
}

// Token is a minted token and when it expires.
type Token struct {
	Value     string
	ExpiresAt time.Time
}

// Hosted is one identity of the host's list.
type Hosted struct {
	Subject string `json:"subject"`
	Ref     string `json:"ref"`
	Name    string `json:"name"`
	Status  string `json:"status"`
}

// Options configure a Client.
type Options struct {
	// URL is TOPOS_IDENTITY_URL, the identity provider's base.
	URL string
	// ClientID is TOPOS_IDENTITY_CLIENT_ID, the host client.
	ClientID string
	// SecretFile is TOPOS_IDENTITY_SECRET_FILE, the file that holds the
	// host client's secret, read at each fetch of the host's token.
	SecretFile string
	// HTTP carries every call but the host's own token, which pkg's
	// instrumented client fetches.
	HTTP *http.Client
	Now  func() time.Time
}

// Client is the identity provider as its host reaches it. It is safe
// for concurrent use.
type Client struct {
	o       Options
	fetch   func(ctx context.Context, url, id, secret string) (string, time.Time, error)
	mu      sync.Mutex
	host    string
	hostExp time.Time
}

// New builds the client.
func New(o Options) (*Client, error) {
	if o.URL == "" || o.ClientID == "" || o.SecretFile == "" {
		return nil, errors.New("identity: the URL, the client id and the secret file are required")
	}
	if o.HTTP == nil {
		return nil, errors.New("identity: no HTTP client")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	o.URL = strings.TrimRight(o.URL, "/")
	return &Client{o: o, fetch: func(ctx context.Context, url, id, secret string) (string, time.Time, error) {
		return oidc.ClientCredentials(ctx, url, id, secret, "", nil)
	}}, nil
}

// hostToken is the host's own token, held until hostTokenMargin before
// its expiry.
func (c *Client) hostToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.host != "" && c.o.Now().Add(hostTokenMargin).Before(c.hostExp) {
		return c.host, nil
	}
	b, err := os.ReadFile(c.o.SecretFile)
	if err != nil {
		return "", fmt.Errorf("identity: read the host's secret: %w", err)
	}
	tok, exp, err := c.fetch(ctx, c.o.URL, c.o.ClientID, strings.TrimSpace(string(b)))
	if err != nil {
		return "", fmt.Errorf("%w: the host's token: %w", ErrUnavailable, err)
	}
	c.host, c.hostExp = tok, exp
	return tok, nil
}

// forget drops the host's token, which the provider no longer accepts.
func (c *Client) forget() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.host = ""
}

// call sends one request as the host and decodes the answer into out.
func (c *Client) call(ctx context.Context, method, path string, in, out any) error {
	tok, err := c.hostToken(ctx)
	if err != nil {
		return err
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.o.URL+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.o.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s %s: %w", ErrUnavailable, method, path, err)
	}
	// The body's close error is the connection's, and no answer is lost.
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("%w: %s %s: %w", ErrUnavailable, method, path, err)
	}
	switch {
	case resp.StatusCode >= 500:
		return fmt.Errorf("%w: %s %s answered %d", ErrUnavailable, method, path, resp.StatusCode)
	case resp.StatusCode >= 300:
		if resp.StatusCode == http.StatusUnauthorized {
			c.forget()
		}
		var env struct {
			Error   string `json:"error"`
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &env) != nil || env.Error == "" {
			return &Error{Status: resp.StatusCode, Code: "unknown", Message: strings.TrimSpace(string(raw))}
		}
		return &Error{Status: resp.StatusCode, Code: env.Error, Message: env.Message}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("identity: %s %s: the answer does not decode: %w", method, path, err)
	}
	return nil
}

func agentPath(ref string) string { return "/hosted-agents/" + url.PathEscape(ref) }

// Create creates the identity of the agent ref, or answers the ref's
// active one with the same owner, and returns its subject.
func (c *Client) Create(ctx context.Context, ref, name string, owner Owner, appliedBy string) (string, error) {
	var out Hosted
	if err := c.call(ctx, http.MethodPut, agentPath(ref), map[string]any{"name": name, "owner": owner, "applied_by": appliedBy}, &out); err != nil {
		return "", err
	}
	if out.Subject == "" {
		return "", fmt.Errorf("identity: the identity of %s came back with no subject", ref)
	}
	return out.Subject, nil
}

// Archive archives the identity of the agent ref. It is permanent: the
// identity is disabled once the agent's last session ends, and the
// confirmation says so.
func (c *Client) Archive(ctx context.Context, ref string) error {
	return c.call(ctx, http.MethodPost, agentPath(ref)+"/archive", map[string]any{"permanent": true}, nil)
}

// Disable disables the identity subject of the agent ref, for good.
func (c *Client) Disable(ctx context.Context, ref, subject string) error {
	return c.call(ctx, http.MethodPost, agentPath(ref)+"/disable", map[string]any{"subject": subject, "permanent": true}, nil)
}

// Archived lists the host's archived identities, every page.
func (c *Client) Archived(ctx context.Context) ([]Hosted, error) {
	var out []Hosted
	after := ""
	for {
		q := url.Values{"status": {"archived"}}
		if after != "" {
			q.Set("after", after)
		}
		var page struct {
			Items      []Hosted `json:"items"`
			NextCursor string   `json:"next_cursor"`
		}
		if err := c.call(ctx, http.MethodGet, "/hosted-agents?"+q.Encode(), nil, &page); err != nil {
			return nil, err
		}
		out = append(out, page.Items...)
		if page.NextCursor == "" || page.NextCursor == after {
			return out, nil
		}
		after = page.NextCursor
	}
}

// Mint asks for a token whose subject is the agent identity subject,
// addressed to audience, carrying the session claim s.
func (c *Client) Mint(ctx context.Context, subject, audience string, s Session) (Token, error) {
	var out struct {
		Token     string `json:"actor_token"`
		ExpiresIn int64  `json:"expires_in"`
	}
	start := c.o.Now()
	in := map[string]any{"audience": audience, "subject": subject, "session": s, "ttl_seconds": int64(maxLifetime / time.Second)}
	if err := c.call(ctx, http.MethodPost, "/actor-tokens", in, &out); err != nil {
		return Token{}, err
	}
	if out.Token == "" || out.ExpiresIn <= 0 {
		return Token{}, errors.New("identity: the identity provider answered no token")
	}
	life := min(time.Duration(out.ExpiresIn)*time.Second, maxLifetime)
	return Token{Value: out.Token, ExpiresAt: start.Add(life)}, nil
}

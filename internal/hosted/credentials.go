// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package hosted

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"latere.ai/x/cella/client"
	cellav1 "latere.ai/x/cella/manifest/v1"
	"latere.ai/x/pkg/otel"

	"latere.ai/x/topos/harness/search"
	"latere.ai/x/topos/internal/websearch"
	"latere.ai/x/topos/machine"
	"latere.ai/x/topos/machine/cella"
	"latere.ai/x/topos/models"
	"latere.ai/x/topos/runner"
	"latere.ai/x/topos/session"
)

// The audiences of the cores a hosted session reaches with its agent's
// tokens (spec 018): Cella for the runner's calls on the session's
// sandbox, and Origo, the git host, for the sandbox's own git.
const (
	AudienceCella = "cella"
	AudienceOrigo = "origo"
)

// The variables a sandbox receives: the placeholders of its Lux key and
// its git host's token, which Cella's egress gateway swaps for their
// values, and the model gateway's URL the key is for.
const (
	EnvLuxKey     = "LUX_KEY"
	EnvLuxURL     = "LUX_URL"
	EnvOrigoToken = "ORIGO_TOKEN"
)

// retryEvery is how soon a sandbox credential that could not be renewed
// is asked for again.
var retryEvery = 30 * time.Second

// credentialSetup is err as the setup error a turn closes with: the
// minter's own code when it named one, agent_identity_missing for an
// agent with no identity, and code otherwise.
func credentialSetup(code string, err error) error {
	if _, coded := errors.AsType[*runner.SetupError](err); coded {
		return err
	}
	if errors.Is(err, runner.ErrNoIdentity) {
		return setup(runner.CodeAgentIdentityMissing, err)
	}
	return setup(code, err)
}

// keyedModel sends every request with the session's own Lux key, asked
// of the drive's token source for each request, so a renewed or replaced
// key reaches the next request and no request carries a key past its
// scope.
type keyedModel struct {
	models.Model
	src *runner.TokenSource
}

func (m *keyedModel) Stream(ctx context.Context, req models.Request) (models.Stream, error) {
	c, err := m.src.Token(ctx, runner.AudienceLux, runner.WorkloadSession)
	if err != nil {
		return nil, fmt.Errorf("the session's model key: %w", err)
	}
	req.Connection.Credential = c.Value
	return m.Model.Stream(ctx, req)
}

// sessionKey is the session's own Lux key for a model connection to the
// installation's model URL, and the model that asks for it afresh on
// each request; ok is false when the installation mints none, and the
// connection keeps the installation's key.
func sessionKey(ctx context.Context, model models.Model) (key string, keyed models.Model, ok bool, err error) {
	src := runner.TokensFrom(ctx)
	if src == nil {
		return "", model, false, nil
	}
	c, err := src.Token(ctx, runner.AudienceLux, runner.WorkloadSession)
	switch {
	case errors.Is(err, runner.ErrNotMinted):
		return "", model, false, nil
	case err != nil:
		return "", nil, false, credentialSetup(CodeModelCredentialMissing, err)
	}
	return c.Value, &keyedModel{Model: model, src: src}, true, nil
}

// searcher is the search service of a session whose agent names
// web_search (spec 047), nil when the installation configures none. It
// searches with the session's own model key for the runner's workload,
// asked of the drive's token source at each search, so a renewed key
// reaches the next one, when the installation mints one, and with
// TOPOS_SEARCH_KEY otherwise. The session's key goes to the
// installation's model URL and to its search URL, both the operator's,
// and to no address an agent names; the sandbox's key is never sent.
func (b builder) searcher(ctx context.Context) (search.Searcher, error) {
	if b.o.SearchURL == "" {
		return nil, nil
	}
	installation := b.o.SearchKey
	cred := func(context.Context) (string, error) { return installation, nil }
	if src := runner.TokensFrom(ctx); src != nil {
		_, err := src.Token(ctx, runner.AudienceLux, runner.WorkloadSession)
		switch {
		case err == nil:
			cred = func(ctx context.Context) (string, error) {
				c, err := src.Token(ctx, runner.AudienceLux, runner.WorkloadSession)
				if err != nil {
					return "", fmt.Errorf("the session's key: %w", err)
				}
				return c.Value, nil
			}
		case !errors.Is(err, runner.ErrNotMinted):
			return nil, credentialSetup(CodeModelCredentialMissing, err)
		}
	}
	c, err := websearch.New(b.o.SearchURL, cred, otel.HTTPClient())
	if err != nil {
		return nil, setup(CodeSearchUnavailable, err)
	}
	return c, nil
}

// cellaToken is the bearer of the runner's own calls to Cella: the
// agent's token for the audience cella and the workload session when the
// installation mints one, else the installation's file.
func cellaToken(ctx context.Context, file client.TokenSource) (client.TokenSource, error) {
	src := runner.TokensFrom(ctx)
	if src != nil {
		_, err := src.Token(ctx, AudienceCella, runner.WorkloadSession)
		switch {
		case err == nil:
			return client.TokenFunc(func(ctx context.Context) (string, error) {
				c, err := src.Token(ctx, AudienceCella, runner.WorkloadSession)
				return c.Value, err
			}), nil
		case !errors.Is(err, runner.ErrNotMinted):
			return nil, credentialSetup(CodeMachineUnavailable, err)
		}
	}
	if file == nil {
		return nil, setup(CodeMachineUnavailable, errors.New("this server mints no Cella token and TOPOS_CELLA_TOKEN_FILE is unset"))
	}
	return file, nil
}

// sandboxSecret is one credential a sandbox uses and never holds: a
// Cella Secret named after the sandbox, mounted under env, scoped to
// host, whose value is the session's credential for audience and the
// workload sandbox, or, for an installation marked so, the
// installation's own credential, which has no expiry to renew before.
type sandboxSecret struct {
	name, env, audience, host string
	labels                    map[string]string
	value                     string
	expires                   time.Time
	installation              bool
}

// hostOf is the host of an absolute URL, lower case.
func hostOf(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("%q names no host", raw)
	}
	return strings.ToLower(u.Hostname()), nil
}

// sandboxSecrets are the secrets of the session's sandbox: its Lux key
// when the installation has session keys, and its git host's token when
// it has an identity provider and a git host, or else its own git host
// credential, OrigoToken, when it has one. Each is applied to Cella
// before the sandbox is opened, so its mount resolves, and is labeled as
// the sandbox is, with the session's and the agent's labels beside the
// installation's, since an authorizer that binds the session's token to
// the session admits only what names it. The installation's model key
// never reaches a sandbox.
func (o CellaOptions) sandboxSecrets(ctx context.Context, s session.Session, c *client.Client) ([]*sandboxSecret, error) {
	src := runner.TokensFrom(ctx)
	if src == nil && o.OrigoToken == nil {
		return nil, nil
	}
	name := cella.SandboxName(s.ID)
	labels := cella.Labels(o.Labels, s.ID, s.Agent.Name)
	var want []*sandboxSecret
	if o.ModelsURL != "" {
		host, err := hostOf(o.ModelsURL)
		if err != nil {
			return nil, setup(CodeModelUnavailable, fmt.Errorf("TOPOS_MODELS_URL %w", err))
		}
		want = append(want, &sandboxSecret{name: name + "-lux", env: EnvLuxKey, audience: runner.AudienceLux, host: host, labels: labels})
	}
	if o.OrigoURL != "" {
		host, err := hostOf(o.OrigoURL)
		if err != nil {
			return nil, setup(CodeMachineUnavailable, fmt.Errorf("TOPOS_ORIGO_URL %w", err))
		}
		want = append(want, &sandboxSecret{name: name + "-origo", env: EnvOrigoToken, audience: AudienceOrigo, host: host, labels: labels})
	}
	var out []*sandboxSecret
	for _, sec := range want {
		cred, err := runner.Credential{}, runner.ErrNotMinted
		if src != nil {
			cred, err = src.Token(ctx, sec.audience, runner.WorkloadSandbox)
		}
		switch {
		case errors.Is(err, runner.ErrNotMinted) && sec.audience == AudienceOrigo && o.OrigoToken != nil:
			v, ferr := o.OrigoToken.Token(ctx)
			if ferr != nil {
				return nil, setup(CodeMachineUnavailable, fmt.Errorf("the git host's credential, TOPOS_ORIGO_TOKEN_FILE: %w", ferr))
			}
			cred, sec.installation = runner.Credential{Value: v}, true
		case errors.Is(err, runner.ErrNotMinted):
			continue
		case err != nil:
			return nil, credentialSetup(CodeMachineUnavailable, err)
		}
		if err := sec.apply(ctx, c, cred); err != nil {
			return nil, setup(CodeMachineUnavailable, err)
		}
		out = append(out, sec)
	}
	return out, nil
}

// apply writes cred as the secret's value, injected as a bearer in the
// Authorization header toward the secret's host alone, with the
// sandbox's labels, the same at every apply.
func (s *sandboxSecret) apply(ctx context.Context, c *client.Client, cred runner.Credential) error {
	body, err := json.Marshal(cellav1.Secret{
		APIVersion: cellav1.APIVersion, Kind: cellav1.KindSecret, Metadata: cellav1.Metadata{Name: s.name, Labels: s.labels},
		Spec: cellav1.SecretSpec{
			Kind: cellav1.SecretStatic, Scope: cellav1.SecretScope{Hosts: []string{s.host}},
			Inject: cellav1.SecretInject{Header: cellav1.DefaultInjectHeader, Scheme: cellav1.SchemeBearer},
			Value:  cred.Value,
		},
	})
	if err != nil {
		return err
	}
	if _, _, err := c.ApplySecret(ctx, s.name, client.JSON(body)); err != nil {
		return fmt.Errorf("apply the sandbox's secret %s: %w", s.name, err)
	}
	s.value, s.expires = cred.Value, cred.ExpiresAt
	return nil
}

// keep renews each secret's value before it expires until ctx ends,
// which is the drive's end, or until stop is called: the source answers a
// fresh credential RefreshBefore its expiry, and a value that changed is
// applied again. A lost lease stops it; any other failure is logged and
// asked again after retryEvery. stop returns once no renewal is in
// flight, so a Secret deleted after it is not applied again.
func keep(ctx context.Context, c *client.Client, src *runner.TokenSource, secrets []*sandboxSecret, log *slog.Logger) (stop func()) {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	for _, sec := range secrets {
		wg.Go(func() {
			for {
				wait := time.Until(sec.expires.Add(-runner.RefreshBefore)) + time.Second
				t := time.NewTimer(max(wait, time.Second))
				select {
				case <-ctx.Done():
					t.Stop()
					return
				case <-t.C:
				}
				cred, err := src.Token(ctx, sec.audience, runner.WorkloadSandbox)
				if err == nil && cred.Value == sec.value {
					sec.expires = cred.ExpiresAt
					continue
				}
				if err == nil {
					err = sec.apply(ctx, c, cred)
				}
				switch {
				case err == nil:
				case errors.Is(err, runner.ErrLeaseLost), ctx.Err() != nil:
					return
				default:
					log.ErrorContext(ctx, "renew a sandbox credential", "secret", sec.name, "err", err)
					sec.expires = time.Now().Add(retryEvery + runner.RefreshBefore)
				}
			}
		})
	}
	return func() {
		cancel()
		wg.Wait()
	}
}

// remove deletes the secrets, a Secret already gone included, and answers
// every delete Cella could not do.
func remove(ctx context.Context, c *client.Client, secrets []*sandboxSecret) error {
	var errs []error
	for _, sec := range secrets {
		if _, err := c.Delete(ctx, client.KindSecret, sec.name); err != nil && client.CodeOf(err) != "not_found" {
			errs = append(errs, fmt.Errorf("delete the sandbox's secret %s: %w", sec.name, err))
		}
	}
	return errors.Join(errs...)
}

// gitConfig points git inside the sandbox at its git host's token: every
// request to the host carries the placeholder as a bearer, which Cella's
// egress gateway swaps for the token. A sandbox without git has nothing
// to configure.
func gitConfig(ctx context.Context, m machine.Machine, origoURL string) error {
	base := strings.TrimRight(origoURL, "/") + "/"
	cmd := `command -v git >/dev/null 2>&1 || exit 0; git config --global "http.` + base + `.extraHeader" "Authorization: Bearer $` + EnvOrigoToken + `"`
	res, err := m.Exec(ctx, machine.ExecRequest{Command: cmd, Timeout: time.Minute})
	switch {
	case err != nil:
		return fmt.Errorf("configure git for %s: %w", origoURL, err)
	case res.ExitCode != 0:
		return fmt.Errorf("configure git for %s: exit %d: %s", origoURL, res.ExitCode, strings.TrimSpace(string(res.Output)))
	}
	return nil
}

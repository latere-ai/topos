# Sandboxes

Every agent's tools run in a sandbox: `bash` runs there, and the file tools
read and write there. A region gets one sandbox, shared by its entry agent and
the steps of a pinned chain, and each delegated peer gets one more of its own.
Each is destroyed when its work ends. `Options.Sandbox` chooses the backend,
any value that implements `sandbox.Provider`, and every agent of the run uses
it. A session's `Turn` is the exception: it runs in a sandbox the host
created, and leaves it in place.

## The default: a local directory

With `Options.Sandbox` unset, the runner uses `sandbox/local`. Each sandbox is
a new temporary directory, removed when the sandbox is destroyed. It needs no
service and is what the examples and the test suite use.

`Options.Workdir` roots that default provider at an existing directory
instead, typically a git worktree, so the agents read and write the host's
files in place. The runner never deletes that directory. `Workdir` is ignored
when `Options.Sandbox` is set.

File operations of the local provider stay inside the directory: a path that
climbs out of it is refused, relative symbolic links that stay inside it work,
and absolute symbolic links are refused. Commands are different. `bash` runs
with the privileges of the host process, and its working directory is checked
before it starts but what the command does is not. The local provider is
therefore for code the host trusts. Untrusted work belongs in an isolated
backend such as Cella.

## Hosted Cella

`sandbox/cella` runs each sandbox in [Cella](https://cella.latere.ai), the
hosted sandbox service:

```go
import (
	"latere.ai/x/topos"
	"latere.ai/x/topos/sandbox"
	"latere.ai/x/topos/sandbox/cella"
)

prov := cella.New(cella.Options{
	BaseURL: "https://cella.latere.ai",
	Token:   cella.ContextTokenSource{}, // the bearer set by sandbox.WithBearer
})
r, err := topos.NewRunner(topos.Options{Sandbox: prov, Model: model})
if err != nil {
	return err
}

// Scope the whole run to one user's Cella identity.
ctx = sandbox.WithBearer(ctx, userBearer)
res, err := r.Run(ctx, region, task)
```

The provider is a plain HTTP client of the service's API and pulls in no Cella
dependency. It speaks the API of the hosted service; the open source Cella
control plane's routes differ, and this provider is not a client of them yet.

Each sandbox it creates is ephemeral unless `CreateOptions.Tier` says
`persistent`, uses the image `ghcr.io/latere-ai/sandbox-base:latest` unless
`CreateOptions.Image` names another, and carries the label `kind=agent`. It
also stops itself after 15 minutes idle, so a sandbox the host forgets to
destroy does not run up cost.

### The bearer

Cella issues no token of its own. The host presents a token its identity
provider minted for Cella, with the audience `sandboxd`, and obtaining that
token and renewing it before it expires is the host's job. The provider asks
its `TokenSource` for the token on every request and stores none, so a renewed
token takes effect on the next request.

| Source | For | When it changes |
|---|---|---|
| `StaticTokenSource("tok")` | one fixed token for the process: a command-line tool, a service account, development | never |
| `TokenFunc(func(ctx) (string, error))` | a token the host holds and renews elsewhere | on every request, so a renewal flows through |
| `ContextTokenSource{}` | a different user per run, set with `sandbox.WithBearer(ctx, tok)` | per context: a long run keeps the token its context carried |

Cella's tokens live for minutes, so a run that can outlast one token needs
`TokenFunc`:

```go
prov := cella.New(cella.Options{
	BaseURL: "https://cella.latere.ai",
	Token: cella.TokenFunc(func(ctx context.Context) (string, error) {
		return tokens.Current(ctx) // the host's cached, renewed token
	}),
})
```

`Options.HTTPClient` replaces the default client, which has no timeout of its
own: deadlines come from the context, which suits commands that run for a long
time.

### Secrets

A secret the workload needs, such as a provider key, never travels as
plaintext in a request. The host stores it in Cella's secret store and refers
to it by name. `CreateOptions.SecretMounts` mounts named secrets as read-only
files under `/run/cella/secrets/<NAME>` when the sandbox starts, and
`ExecOptions.SecretEnv` places one in a single command's environment,
resolved by the service rather than passed on the command line:

```go
create := sandbox.CreateOptions{SecretMounts: []string{"OPENAI_API_KEY"}}

exec := sandbox.ExecOptions{
	Argv:      []string{"deploy"},
	SecretEnv: map[string]string{"OPENAI_API_KEY": "openai_key"}, // variable -> secret name
}
```

A nil `SecretMounts` mounts the caller's default set, and an empty slice mounts
none. `Env` remains the place for configuration that is not secret. The local
provider has no secret store and ignores both fields.

## Guarding a provider

Two wrappers in the `sandbox` package add policy around any provider.

`sandbox.Confine(inner, root)` refuses file paths, and explicit working
directories of commands, that leave `root` or that match a fixed deny-list of
credential files: `.env` and `.env.*`, `*.pem`, `id_rsa`, `id_dsa`,
`id_ecdsa`, and `id_ed25519` keys, `.netrc`, anything under a `.ssh`
directory, and `.aws/credentials`. The list cannot be turned off. A provider
that can resolve symbolic links, as the local one can, has the link's target
checked as well. A refusal matches `sandbox.ErrConfined`.

`sandbox.Consent(inner, decide)` calls `decide` before every command and
refuses the command when it returns an error, which then matches
`sandbox.ErrConsentDenied`. It is where a host asks a person before a command
runs on their machine. A nil `decide` allows everything.

Neither is applied by default; a host composes them around the provider it
passes in.

## A remote machine as a sandbox

`sandbox/rpc` carries the provider interface over any byte stream.
`rpc.Serve` exposes a provider on one end of a connection and `rpc.NewClient`
returns a provider on the other, so a control plane can drive a machine it
cannot reach directly, such as a developer's laptop that dialed out through a
tunnel. The machine that owns the files decides what is allowed:

```go
// On the machine being driven:
host := local.NewAt(repoDir)
err := rpc.Serve(ctx, conn, sandbox.Consent(sandbox.Confine(host, repoDir), askUser))

// In the control plane:
r, err := topos.NewRunner(topos.Options{Sandbox: rpc.NewClient(conn), Model: model})
```

One request is in flight per connection at a time. A transport that needs
several streams multiplexes them below this layer.

## A backend of the host's own

Any type that implements `sandbox.Provider` works as `Options.Sandbox`. The
interface is eight methods: create, destroy, run a command to completion,
stream a command's output, read, write, and list files, and a health check. It
must be safe for concurrent use, and its errors follow one contract:
`sandbox.ErrNotFound` for a missing sandbox, `sandbox.ErrConflict` for a name
collision, and `*sandbox.APIError` for any other backend failure.
[`examples/sandbox`](../examples/sandbox) switches between the local provider
and Cella with one environment variable.

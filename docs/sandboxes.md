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

`sandbox/cella` runs each sandbox on a [Cella](https://github.com/latere-ai/cella)
control plane, the open source sandbox runtime Latere hosts at
`https://api.latere.ai/v1/environments`:

```go
import (
	"latere.ai/x/topos"
	"latere.ai/x/topos/sandbox"
	"latere.ai/x/topos/sandbox/cella"
)

prov := cella.New(cella.Options{
	BaseURL: "https://api.latere.ai/v1/environments",
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

`BaseURL` is a control plane's address including the path it is served under,
so the same provider reaches a Cella control plane run elsewhere. The provider
is built on Cella's exported Go client, `latere.ai/x/cella/client`, which
brings the standard library and Cella's manifest types into the build and none
of the control plane's own code.

### What a sandbox gets

| Setting | Value |
|---|---|
| Image | `CreateOptions.Image`, else `base`. The hosted control plane runs images from its catalog only: `base`, and `gui` for a desktop |
| Label | `kind=agent`, beside the caller's `CreateOptions.Labels` |
| Idle stop | after 15 minutes without activity, so a sandbox the host forgets to destroy does not run up cost; the workspace survives a stop |
| Tier | `ephemeral`, the default, is also deleted 24 hours after it was created; `persistent` is kept until it is deleted |
| Network | `allowlist` with `api.latere.ai` alone, unless `Options.AllowedHosts` names the hosts |

`Create` holds the request until the sandbox runs, bounded by the context's
deadline, so the first command can follow at once. A sandbox that fails to
start is deleted, and `Create` returns an error naming it and the reason. The
`Tier` a sandbox reports is read from the control plane's answer: an
installation that deletes every sandbox after a fixed time reports each as
`ephemeral`.

### The network boundary

A hosted sandbox reaches only the hosts its boundary admits, and an
organization's sandboxes are held to an allowlist whatever the request asks.
The default list is the Latere API origin, where the hosted models are served,
so an agent placed in a sandbox can still call its model. `Options.AllowedHosts`
replaces that list; each entry is an exact host name or one leading `*.`
wildcard:

```go
prov := cella.New(cella.Options{
	BaseURL:      "https://api.latere.ai/v1/environments",
	Token:        src,
	AllowedHosts: []string{"api.latere.ai", "github.com", "*.githubusercontent.com"},
})
```

A control plane with no egress gateway connected refuses an allowlist: the
create returns `*sandbox.APIError` with the code `egress_gateway_unavailable`.

### Commands and files

A command runs to completion, and its standard input is at end of file.
`ExecResult.Stdout` holds its standard output followed by its standard error;
the control plane keeps the first mebibyte of each. The timeout sent with it is
the context's remaining time, at most an hour, and an hour when the context has
no deadline. A context that ends first reports the `killed` phase. `StreamExec`
delivers the same output as one chunk when the command ends.

A relative path, in a file operation or in `ExecOptions.Cwd`, is resolved under
the workspace, `/workspace`. An absolute path is used as given, and one outside
the workspace is refused.

### The bearer

Cella issues no token of its own. The host presents a token its identity
provider minted for an audience the control plane accepts, and obtaining that
token and renewing it before it expires is the host's job. The provider asks
its `TokenSource` for the token on every request and stores none, so a renewed
token takes effect on the next request.

| Source | For | When it changes |
|---|---|---|
| `StaticTokenSource("tok")` | one fixed token for the process: a command-line tool, a service account, development | never |
| `TokenFunc(func(ctx) (string, error))` | a token the host holds and renews elsewhere | on every request, so a renewal flows through |
| `ContextTokenSource{}` | a different user per run, set with `sandbox.WithBearer(ctx, tok)` | per context: a long run keeps the token its context carried |

Hosted tokens live for minutes, so a run that can outlast one token needs
`TokenFunc`:

```go
prov := cella.New(cella.Options{
	BaseURL: "https://api.latere.ai/v1/environments",
	Token: cella.TokenFunc(func(ctx context.Context) (string, error) {
		return tokens.Current(ctx) // the host's cached, renewed token
	}),
})
```

`Options.HTTPClient` replaces the default client, which has no timeout of its
own: deadlines come from the context. A client with a `Timeout` shorter than a
sandbox's start or a command's run cuts that request off.

### What the Cella backend refuses

Three fields have no counterpart on the control plane, and each asks for a
restriction or a credential, so the provider refuses a request that sets one
rather than run the work without it:

| Field | Why |
|---|---|
| `CreateOptions.Policy` | the control plane has no named policies; a sandbox's boundary is its own manifest |
| `CreateOptions.SecretMounts` | a Cella secret reaches a sandbox as a placeholder its egress gateway replaces on the way to the secret's hosts, never as a file holding the value; nil and empty mount nothing |
| `ExecOptions.SecretEnv` | the exec route resolves no secret for one command |

`Env` remains the place for configuration that is not secret.

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

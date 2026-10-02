# Configuration

`toposd` reads its configuration from `TOPOS_*` variables once at start-up.
A start-up with anything missing or malformed exits 1 with one line that
names every problem. This page lists the variables read today; the
`topos` command's own are under [The topos command](#the-topos-command).

| Variable | Default | Meaning |
|---|---|---|
| `TOPOS_PUBLIC_ADDR` | `:8080` | the listener for the API and the public probes |
| `TOPOS_INTERNAL_ADDR` | `:8081` | the listener for the cluster's probes and, with `TOPOS_RUNNER_TOKEN`, the routes runners claim sessions from; `toposd runner` serves its probes on it. Two addresses that name one socket stop the start |
| `TOPOS_PUBLIC_URL` | required | the absolute URL clients reach the API root at, and the name of the local issuer; every URL toposd writes (a session's stream `Link`, the next page's `Link`, the served `openapi.yaml`'s server) starts with it, whatever host a request named. Its path is `TOPOS_BASE_PATH`, and it has none when that is unset |
| `TOPOS_BASE_PATH` | unset | the path the API answers under in the place of `/v1`, for a server mounted under a prefix of an origin it shares with other services: with `/v1/agents`, agents are at `/v1/agents/agents`, sessions at `/v1/agents/sessions` and the document at `/v1/agents/openapi.yaml`. It starts with `/`, has no trailing `/`, and equals the path of `TOPOS_PUBLIC_URL`; a mismatch stops the start. A path outside it answers `not_found`, and the probes and the build identity stay at the listener's root. Unset, the API answers under `/v1` |
| `TOPOS_OIDC_ISSUERS` | required unless `TOPOS_LOCAL_ISSUER_KEY` is set | comma-separated issuer URLs whose tokens are accepted; each must answer at start |
| `TOPOS_OIDC_AUDIENCE` | `topos` | comma-separated audiences a token may carry, the first the primary |
| `TOPOS_OIDC_INSECURE_ISSUERS` | unset | listed issuers allowed to use `http://` on a host other than loopback |
| `TOPOS_AUTHORIZER_URL`, `TOPOS_AUTHORIZER_TOKEN` | unset | the installation's authorizer and its bearer; unset, the owner policy decides |
| `TOPOS_ADMIN_SUBJECTS` | unset | subjects (`<issuer>\|<sub>`) the owner policy lets act on every object |
| `TOPOS_LOCAL_ISSUER_KEY` | unset | a PEM PKCS#8 ECDSA P-256 or RSA (2048 bits or more) private key; set, toposd accepts the tokens `toposd token` signs and serves its key set at `<TOPOS_PUBLIC_URL>/.well-known/jwks.json` |
| `TOPOS_DB_URL`, `TOPOS_DB_POOL_URL` | unset | Postgres for sessions, and an optional transaction-pooling URL for queries; unset, agents and sessions are kept in `TOPOS_DATA_DIR` |
| `TOPOS_DATA_DIR` | `$XDG_STATE_HOME/topos`, or `$HOME/.local/state/topos` | the data directory: the agents and sessions of a server without `TOPOS_DB_URL`, which one serving toposd holds alone, and the session directories of `TOPOS_HOST_SESSIONS`. `toposd runner` reads it only with `TOPOS_HOST_SESSIONS=on`. With neither it nor `XDG_STATE_HOME` nor `HOME` set, the start stops |
| `TOPOS_BLOB_URL` | unset | where raw model responses and captured requests are kept: `file:///<path>`, or `s3://<host>/<bucket>/<prefix>` for an S3 compatible object store, with `?region=` when it is not `us-east-1`; unset keeps them in the database or the data directory |
| `TOPOS_BLOB_ACCESS_KEY`, `TOPOS_BLOB_SECRET_KEY` | required with an `s3://` URL | the object store's access key and secret; there is no credential chain |
| `TOPOS_MODELS_URL` | required | the model connection of an agent that names none: a Lux gateway's root, such as `https://lux.example/v1/models`, from which each model reaches its own family's door; one door of it, such as `https://lux.example/v1/models/anthropic`; or a provider's API. toposd asks a URL that names no door for Lux's discovery document at start, and one that does not answer stops the start. The runners reach each door under this URL, so a Lux root inside the installation's network, such as its in-cluster Service, keeps every model request there, while a sandbox's `LUX_URL` is the root Lux published its doors under, since a sandbox reaches only public hosts. Through a Lux door, each model's window, output limit and prices come from the door's model list where it gives them, and a door that does not answer the list fails the turn with `model_unavailable`. A session's create and a switch of its model check the model by the same route: with `TOPOS_MODELS_KEY` the door's list is read with it, and without it, where each session reads the list with its own key, a model that goes through a Lux door passes and its figures are read at the turn |
| `TOPOS_MODELS_KEY` | unset | the credential sent with `TOPOS_MODELS_URL`; a session whose agent names no credential fails its turn with `model_credential_missing` without it; refused beside `TOPOS_SESSION_KEYS_URL`, whose sessions use their own keys |
| `TOPOS_RUNNER_CAPACITY` | `16` | how many hosted sessions the server, or one `toposd runner`, drives at once; `0` runs none on the server and stops a runner's start |
| `TOPOS_RUNNER_TOKEN` | unset | comma-separated bearers of the routes runners claim sessions from. Set on the server, its internal listener answers them under `/internal/v1` and accepts every bearer listed, so a new one is rolled out beside the old; unset, it answers no runner. `toposd runner` requires it and sends the first |
| `TOPOS_INTERNAL_URL` | required by `toposd runner` | the absolute `http` or `https` URL of the server's internal listener a runner claims sessions from, such as `http://toposd.example:8081` |
| `TOPOS_CELLA_URL`, `TOPOS_CELLA_TOKEN_FILE` | unset | the Cella control plane hosted sessions' sandboxes are created on, and the file holding the bearer presented to it, read on every request; a session's sandbox is created when its agent first calls a tool that acts on a machine, and without a URL that call answers `machine_unavailable`. With it set and `TOPOS_HOST_SESSIONS` off, a session of an agent that names no machine, or `kind: host` alone, runs in a sandbox of the default image, since the manifest's `host` default is a local run's. With `TOPOS_IDENTITY_URL` the file is refused, since each session reaches Cella with its agent's token; `toposd runner` presents it only for a session its server mints no Cella token for |
| `TOPOS_CELLA_LABELS` | unset | comma-separated `key=value` labels that every sandbox, and every Secret toposd applies for one, carries beside the session's and the agent's `topos.latere.ai/` labels, for a Cella whose authorizer places what a caller creates by labels, such as a tenant and the caller's own id. The same labels are sent at every apply. A key under `topos.latere.ai/`, a key given twice, or a pair that is not `key=value` stops the start |
| `TOPOS_ORIGO_URL` | unset | the git host a hosted session's sandbox clones from and pushes to: the sandbox holds a placeholder, `ORIGO_TOKEN`, that Cella's egress gateway swaps for a credential on requests to that host, and its git sends it. With `TOPOS_IDENTITY_URL` the credential is the agent's token; without one it is `TOPOS_ORIGO_TOKEN_FILE`'s, and with neither the sandbox's git sends none. A session whose first repository is a private one on this host keeps each turn's checkpoint there, under `refs/topos/checkpoints/<session>/latest`, so a session that continues it gets its files after its sandbox is gone; private means the host refuses a read with no credential for want of one, and a public repository, or one whose answer says nothing clear, never gets a checkpoint, since its readers would read the session's uncommitted files. The ref stays until the repository or the ref is deleted |
| `TOPOS_ORIGO_TOKEN_FILE` | unset | the file holding the git host credential of an installation without an identity provider, such as a repository token with push access, which every sandbox's git sends to the host of `TOPOS_ORIGO_URL` in the place of an agent's token. It is read at each sandbox's open, so it can be rotated in place, and never enters the sandbox. Needs `TOPOS_ORIGO_URL`; refused beside `TOPOS_IDENTITY_URL` |
| `TOPOS_IDENTITY_URL`, `TOPOS_IDENTITY_CLIENT_ID`, `TOPOS_IDENTITY_SECRET_FILE` | unset | the identity provider that hosts this installation's agents, the client toposd authenticates as there, and the file holding that client's secret, read at each fetch of its token. Set, every agent gets an identity at its first apply, owned by the organization the authorizer names for it, or else by the applier; archiving the agent archives its identity, which is disabled for good once the agent's last session ends; and each session reaches the other services with short tokens minted for its agent and naming the session. Needs `TOPOS_AUTHORIZER_URL` |
| `TOPOS_SESSION_KEYS_URL`, `TOPOS_SESSION_KEYS_TOKEN` | unset | the authorizer's session key routes and their bearer. Set, each hosted session asks models with a key of its own, and its sandbox with a second, `LUX_KEY` beside `LUX_URL`, both generated by toposd, registered by their SHA-256 alone, and renewed while the session runs. Needs `TOPOS_AUTHORIZER_URL` |
| `TOPOS_HOST_SESSIONS` | `off` | `on` runs a hosted session whose agent's machine is `host` on the server's own host, an agent that names no machine included; `off` refuses such a session with `machine_unavailable`, except that an agent that names no machine runs on Cella when `TOPOS_CELLA_URL` is set. With it on, every command runs inside the host sandbox (`srt`: Seatbelt on macOS, Bubblewrap, `socat` and `ripgrep` on Linux) in a directory of the session's own under `TOPOS_DATA_DIR/host-sessions/`, reads nothing else in the data directory and no configured file, gets only `PATH`, `LANG`, `TZ`, `TERM` and `HOME` of the environment, and reaches only the agent's `machine.egress` hosts; an agent that names `machine.roots` or `machine.readPaths` is refused. `serve` and `runner` refuse to start when the sandbox is not installed, when a probe command cannot run inside it, or when the probe can read the data directory; the server needs a `HOME` of its own, which the sandbox denies to every command |
| `TOPOS_MACHINE_HELPERS`, `TOPOS_MACHINE_DIR` | `/usr/local/lib/topos`, `/tmp/topos` | where the helper builds a sandbox receives are, and where inside the sandbox they go |

## Roles

| Command | Today |
|---|---|
| `toposd` or `toposd serve` | serves the API under `/v1`, or `TOPOS_BASE_PATH`, on the public listener, `/livez`, `/readyz` and `/version` on both listeners, the build identity at `/`, and the local issuer's key set when there is one; drives up to `TOPOS_RUNNER_CAPACITY` hosted sessions itself, ends expired sessions and fires triggers, and with `TOPOS_RUNNER_TOKEN` hands sessions to runners on the internal listener |
| `toposd runner` | claims hosted sessions from `TOPOS_INTERNAL_URL` with the first of `TOPOS_RUNNER_TOKEN` and drives up to `TOPOS_RUNNER_CAPACITY` of them at once; reads the model and machine variables, keeps nothing of its own, and serves only `/livez`, `/readyz` and `/version` on `TOPOS_INTERNAL_ADDR` |
| `toposd check` | exits 1: spec 028 builds it |
| `toposd token` | signs a token with `TOPOS_LOCAL_ISSUER_KEY` and prints it: `--subject` (`admin`), `--ttl` (`1h`, at most `24h`), `--audience` (the first of `TOPOS_OIDC_AUDIENCE`); opens no store |
| `toposd -version` | prints the build identity |

Exit codes: 0 on a clean stop, 1 on a start-up or runtime failure, 2 on a
usage error.

## Running without an identity provider

A self-hoster runs toposd as its own issuer: set `TOPOS_LOCAL_ISSUER_KEY`,
leave `TOPOS_AUTHORIZER_URL` unset so the owner policy decides, and set
`TOPOS_ADMIN_SUBJECTS` to the subject of the token `toposd token` prints,
`<TOPOS_PUBLIC_URL>|admin` by default. `make run` does this on loopback
with a key it generates under `out/`, and `make token` prints a token for
it.

Every session then acts with the installation's own credentials:
`TOPOS_MODELS_KEY` for models, the file of `TOPOS_CELLA_TOKEN_FILE` for
Cella, and the file of `TOPOS_ORIGO_TOKEN_FILE` for the git host its
sandboxes clone from and push to. Each file is read again when it is
used, so whatever renews a credential rewrites its file in place. The
sandbox gets no model key of its own. A Cella that places each object in
its creator's tenant by labels gets them from `TOPOS_CELLA_LABELS`.

## The topos command

`topos run`, `topos confirm` and `topos rewind` run a session on the
machine they start on, with no server, and read these variables at each
invocation.

| Variable | Default | Meaning |
|---|---|---|
| `TOPOS_MODELS_URL`, `TOPOS_MODELS_KEY` | unset | the model connection and its credential of an agent that names no base URL, as for `toposd`: a Lux gateway's root, whose discovery document is read when the first model connects, one door of it, or a provider's API. A turn of such an agent without the URL exits 2 |
| `TOPOS_DATA_DIR` | `$XDG_STATE_HOME/topos`, or `$HOME/.local/state/topos` | where the sessions are kept, with the worktree a session gets when another session that has not ended writes its checkout |
| `TOPOS_DECISIONS_URL`, `TOPOS_DECISIONS_TOKEN` | unset | a decision service and the bearer sent to it. Set, the service suggests a verdict on each tool call: in `auto` mode its suggestion decides the calls the rules leave open, with a share of those automatic verdicts drawn for a person to review; in `manual` mode the rules decide; in `plan` mode it is not asked. Every decision and every confirmation is sent to it. A service that fails, or does not answer within two seconds, makes the call wait for a person. A URL that is not `http` or `https`, or one without its token, exits 2 |
| `TOPOS_URL` | unset | reserved for the server `topos` will reach once it runs server sessions; set, `topos run` exits 2, since it runs local sessions only |

## Variables this page leaves out

The task suite reads `TOPOS_MODELS_URL`, `TOPOS_MODELS_KEY` and its own
`TOPOS_TASKS_*` only under `go test -tags tasks ./test/tasks/`; they are
listed with the suite in [its spec](../specs/025-task-suite.md#running-the-suite-from-elsewhere).
Topos sets `TOPOS_FETCH_*` for the fetch command it runs in a sandbox,
and `TOPOS_TASK` and `TOPOS_SERVE_URL` for a task's `check.sh`; none of
them is read from the environment toposd or `topos` starts in.

# Running Topos locally

This page runs `toposd` on one machine from a clone, with every
dependency it needs, and ends with a session that answers a message. It
needs no provider key, no database and no sandbox service: the model is
the stand-in providers of [Lux](https://github.com/latere-ai/lux), the
store is a directory, and the session makes no tool call. Each section
after the first run says what the commands did and what to change to
use something real. [Installing Topos](install.md) covers a server that
others use.

Everything here is for trying Topos on loopback. The stand-in providers
answer with a fixed string, and the server signs its own tokens.

## What you need

Install these first: Go 1.27 or later (`go.mod` names the version),
`make`, `openssl`, `curl` and `git`. A container engine is needed only
for [Postgres](#postgres-instead-of-the-directory).

Nothing else may answer on `127.0.0.1:8080` or `127.0.0.1:8081`, which
`make run` uses. Check, before you start:

```sh
curl -s -m 2 -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080
curl -s -m 2 -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8081
```

Each prints `000` when nothing is listening. Any other number is
another server, and the requests below would reach it.

## From a clone to a reply

Run this in one shell. It leaves two servers running in the background,
so use the same shell for the sections after it.

```sh
git clone https://github.com/latere-ai/lux.git
git clone https://github.com/latere-ai/topos.git
cd lux
eval "$(make run | grep '^export ')"
cd ../topos
export TOPOS_MODELS_URL=$LUX_URL TOPOS_MODELS_KEY=$LUX_KEY TOPOS_DATA_DIR=$PWD/out/data
mkdir -p out
make run > out/toposd.log 2>&1 &
until curl -sf http://127.0.0.1:8080/readyz > /dev/null; do sleep 1; done
TOKEN=$(make token)
curl -sS -X PUT http://127.0.0.1:8080/v1/agents/hello \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/yaml' \
  --data-binary @- <<'YAML'
apiVersion: topos.latere.ai/v1
kind: Agent
metadata:
  name: hello
spec:
  model:
    name: stub-anthropic
YAML
SESSION=$(curl -sS -X POST http://127.0.0.1:8080/v1/sessions \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"agent":"hello","message":"hello"}' | sed -n 's/.*"id":"\(ses_[^"]*\)".*/\1/p')
until curl -sSL http://127.0.0.1:8080/v1/sessions/$SESSION/events \
  -H "Authorization: Bearer $TOKEN" | grep -q '"status":"idle"'; do sleep 1; done
curl -sSL http://127.0.0.1:8080/v1/sessions/$SESSION/events \
  -H "Authorization: Bearer $TOKEN" |
  sed -n 's/.*"type":"agent.message".*"blocks":\[{"type":"text","text":"\([^"]*\)".*/\1/p'
```

The last command prints the agent's reply:

```text
stub:anthropic:stub-anthropic:2cf24dba
```

The stand-in answers `stub:<dialect>:<model>:<digest>`, where the
digest is the start of the SHA-256 of your last message. The first
build downloads the Go modules. The commands use no `jq`; the two `sed`
calls read one field each.

## What the commands did

1. **Lux.** `make run` in the Lux clone builds and starts the gateway
   and its stand-in providers in the background, applies its example
   models, and prints three `export` lines: `LUX_URL`, `LUX_KEY` and
   `LUX_TOKEN`. The `eval` puts them in your shell. Its models are
   `stub-openai` and `stub-anthropic`, and its model list gives each the
   input window and output limit Topos needs for every model. [Running Lux
   locally](https://github.com/latere-ai/lux/blob/main/docs/local.md)
   says what else it starts.
2. **The model connection.** `TOPOS_MODELS_URL` is Lux's root, the
   address `LUX_URL` holds, and `TOPOS_MODELS_KEY` is its key.
   `TOPOS_DATA_DIR` keeps the server's data inside the clone, under
   `out/data`. At start
   `toposd` asks that root for Lux's discovery document and learns the
   address of each door. A start with no `TOPOS_MODELS_URL` exits 1 with
   one line, `toposd: configuration: TOPOS_MODELS_URL is required; ...`,
   and [Configuration](configuration.md) says a root that does not
   answer stops the start too.
3. **The server.** `make run` builds `out/toposd` and `out/topos`,
   generates the issuer key if there is none, and runs the server in the
   foreground, so the command is sent to the background and its log is
   `out/toposd.log`. [What `make run` sets](#what-make-run-sets) lists
   its settings. `/readyz` answers when the server is up.
4. **The token.** `make token` builds if it must, then prints a
   token for the admin subject and nothing else on standard output.
5. **The agent and the session.** `PUT /v1/agents/hello` applies an
   agent, and `POST /v1/sessions` creates a session of it with a first
   message. The turn runs in the background, so the loop waits for the
   session's `idle` status and then reads its events. The
   [API guide](api.md#flows) covers the flows, and
   `GET /v1/openapi.yaml` is the reference.

The agent names no machine. The server has no sandbox service, and
the session needs none: a sandbox is made only when a tool needs one
([A machine for tool calls](#a-machine-for-tool-calls)), and the
stand-ins never ask for a tool.

The reply sits in the log with the model request that made it. To read
the whole log, drop the last `sed`; it is one JSON document, with
`user.message`, `session.status`, `model.request` (the model, the
tokens, the cost), `agent.message` and a closing `session.status` of
`idle`. The events are described in the [API guide](api.md#flows).

## What `make run` sets

`make run` is a shell line that sets these and starts `out/toposd`:

| Setting | Value | Why |
|---|---|---|
| `TOPOS_PUBLIC_URL` | `http://127.0.0.1:8080` | the URL clients reach the API at, and the name of the local issuer |
| `TOPOS_LOCAL_ISSUER_KEY` | the contents of `out/local-issuer.pem` | the server is its own identity provider: it signs the token `make token` prints and serves the matching public key at `/.well-known/jwks.json` |
| `TOPOS_ADMIN_SUBJECTS` | `http://127.0.0.1:8080\|admin` | the issuer and subject of that token, which the owner policy lets act on every object |
| `TOPOS_PUBLIC_ADDR`, `TOPOS_INTERNAL_ADDR` | `127.0.0.1:8080`, `127.0.0.1:8081` | the API and the probes, reachable from this machine only |

It sets no `TOPOS_AUTHORIZER_URL`, so the built-in owner policy decides
every action: the admin may do anything, and anyone else may act on the
objects they own ([the rows](authorizer.md#without-an-authorizer)). It
sets no model variables and no data directory, which is why the
commands above export them.

The issuer key is made once, with
`openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out out/local-issuer.pem`,
and every later `make run` and `make token` reuses it. Deleting it
makes the next run generate another, and every token the old one signed
stops working ([Keys](install.md#keys)). `make token` signs with the
same key and the same settings, so its token is accepted by the running
server. How long a token lasts, and the flags that change its subject
and lifetime, are on the `toposd token` row of
[Configuration](configuration.md#roles).

The full list of variables, with their defaults, is
[Configuration](configuration.md). Running without an identity provider
is [its own section](configuration.md#running-without-an-identity-provider).

## The model connection

`toposd` does not start without one. Three choices:

- **Lux's stand-ins**, as above. Nothing leaves the machine.
- **A real Lux gateway.** Set `TOPOS_MODELS_URL` to its root and
  `TOPOS_MODELS_KEY` to a key it issued. The agent then names a model
  that gateway lists, and the figures of the model come from its list.
- **A provider's API directly.** Set `TOPOS_MODELS_URL` to the provider's
  API address and `TOPOS_MODELS_KEY` to its key, and name a model the
  built-in catalog knows or give its figures in the agent.

An agent can name its own base URL, which replaces the server's for
that agent. [Configuration](configuration.md) has the rows for
`TOPOS_MODELS_URL` and `TOPOS_MODELS_KEY`, including what a Lux root
does that a single door does not.

## Where data is kept

With no `TOPOS_DB_URL`, agents and sessions are files under
`TOPOS_DATA_DIR`, here `out/data`. The log line at start names the
directory:

```text
toposd: v0.70.0-9-g4fc338c listening public=127.0.0.1:8080 internal=127.0.0.1:8081 store=dir:/home/you/topos/out/data
```

The directory holds `objects/`, `sessions/` and a `serve.lock`, which
keeps a second `toposd` off the same directory. [Configuration](configuration.md)
says where the default is when the variable is unset, and what else
the directory holds. A restart on the same directory keeps the agents
and sessions, and a token stays valid, because the issuer key is under
`out/`, not in the data directory.

### Postgres instead of the directory

Set `TOPOS_DB_URL` and the server keeps sessions in Postgres instead.
It applies its own migrations at start, so the database only has to be
empty and reachable ([The database](install.md#the-database)). With a
container for Postgres 17, the version the store's tests run against:

```sh
podman run -d --rm --name topos-postgres -p 127.0.0.1:25432:5432 \
  -e POSTGRES_PASSWORD=topos -e POSTGRES_DB=topos docker.io/library/postgres:17-alpine
until podman exec topos-postgres pg_isready -h 127.0.0.1 -U postgres -d topos > /dev/null; do sleep 1; done
TOPOS_DB_URL='postgres://postgres:topos@127.0.0.1:25432/topos?sslmode=disable' \
  make run > out/toposd.log 2>&1 &
```

The start line then ends with `store=postgres`. The agent and session
commands above work the same against it, and the session's five events
are rows of `events`:

```sh
podman exec topos-postgres psql -U postgres -d topos -tc 'select count(*) from events'
```

```text
     5
```

The `pg_isready` loop asks over TCP, which the container answers only
once the database is up. The data directory is still used for a few
files on a server on Postgres ([the data
directory](install.md#the-data-directory-and-the-blob-store)).

## A machine for tool calls

A session's tools that act on a machine (`bash`, `read`, `write`, `edit`
and the others the agent lists) need one. The server has three places to
get it:

- **A Cella.** [Cella](https://github.com/latere-ai/cella) makes a
  sandbox for the session. Set `TOPOS_CELLA_URL` and
  `TOPOS_CELLA_TOKEN_FILE`; an agent that names no machine then runs in
  a sandbox of the default image. Where the sandbox may connect is
  [A session's network](network.md).
- **The server's own host.** `TOPOS_HOST_SESSIONS=on` runs a session
  whose machine is `host` on the machine the server runs on, with its
  commands inside the host sandbox. The server refuses to start when
  that sandbox is not installed. The rows for `TOPOS_HOST_SESSIONS` and
  `TOPOS_CELLA_URL` in [Configuration](configuration.md) list what each
  needs, and [Security](security.md) says what each confines.
- **Your own computer, with no server.** [`topos run`](#topos-run-without-a-server).

With none of them, a session still runs as long as the model asks for no
tool; the Lux stand-ins never ask for one. When a model asks for a
`bash` call on a server with no `TOPOS_CELLA_URL` and
`TOPOS_HOST_SESSIONS` off, the call does not run. The session logs a
`session.error` with the code `machine_unavailable`, then a `tool.result`
marked as an error:

```text
The machine this tool acts on could not be started, so the call did not run: machine_unavailable: TOPOS_CELLA_URL is unset, so this server has no machines
```

The model reads that text as the tool's result and answers, and the
turn ends with the session `idle` and the stop reason `end_turn`. The
server does not fail the session or the create.

## `topos run` without a server

`topos run` runs a turn of a local session in the working directory and
needs no `toposd`. It reads the same model variables, which the shell
still holds from the first section:

```sh
mkdir -p out/work && cd out/work
cat > agent.yaml <<'YAML'
apiVersion: topos.latere.ai/v1
kind: Agent
metadata:
  name: hello
spec:
  model:
    name: stub-anthropic
  instructions: Answer briefly.
YAML
TOPOS_DATA_DIR=$PWD/data ../topos run --agent agent.yaml "hello"
cd ../..
```

```text
stub:anthropic:stub-anthropic:2cf24dba
```

The agent has no `machine`, because a local run's default is the
machine it runs on. `--output json` prints the session id, its status
and the reply instead. The session is kept in `TOPOS_DATA_DIR`, here
`out/work/data`. The other flags are in `topos run -h`, and the
variables it reads are [The topos command](configuration.md#the-topos-command).

## Stop and clean up

Stop the three things you started and confirm the ports are free.
`make run` execs `out/toposd`, so the server is the `make` job's own
child: stopping the server ends the job, and stopping the job (`kill`
with the job number `jobs` shows in the shell that started it) stops the
server.

```sh
pkill -f '^out/toposd$'
make -C ../lux run-down
podman stop topos-postgres
```

The last line only applies if you started Postgres. The server answers
`/readyz` with 503 as soon as it gets the signal and closes its ports
3 seconds later, its drain delay, so the port check below prints `000`
for 8080 only after that.

`make clean` removes `out/`, with the data directory, the issuer key,
the log and the build; then the two clones can go:

```sh
make clean
cd .. && rm -r lux topos
```

Check that nothing is left:

```sh
curl -s -m 2 -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080
curl -s -m 2 -o /dev/null -w '%{http_code}\n' "$LUX_URL"
```

Both print `000`.

## Where next

- [Installing Topos](install.md) for an image, a database and keys for a
  server others use.
- [Building on the Topos API](api.md) for what a client does with the
  sessions: streaming, questions, forks and triggers.
- [Writing an authorizer](authorizer.md) to replace the owner policy.
- [Configuration](configuration.md) for every variable.

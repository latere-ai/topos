# Configuration

`toposd` reads its configuration from `TOPOS_*` variables once at start-up.
A start-up with anything missing or malformed exits 1 with one line that
names every problem. Spec [002](../specs/002-scaffold-and-configuration.md)
is the reference for every variable; this page lists the ones read today.

| Variable | Default | Meaning |
|---|---|---|
| `TOPOS_PUBLIC_ADDR` | `:8080` | the listener for the API and the public probes |
| `TOPOS_INTERNAL_ADDR` | `:8081` | the listener for the cluster's probes, metrics and, later, the runners |
| `TOPOS_PUBLIC_URL` | required | the absolute URL clients reach the public listener at, and the name of the local issuer |
| `TOPOS_OIDC_ISSUERS` | required unless `TOPOS_LOCAL_ISSUER_KEY` is set | comma-separated issuer URLs whose tokens are accepted; each must answer at start |
| `TOPOS_OIDC_AUDIENCE` | `topos` | comma-separated audiences a token may carry, the first the primary |
| `TOPOS_OIDC_INSECURE_ISSUERS` | unset | listed issuers allowed to use `http://` on a host other than loopback |
| `TOPOS_AUTHORIZER_URL`, `TOPOS_AUTHORIZER_TOKEN` | unset | the installation's authorizer and its bearer; unset, the owner policy decides |
| `TOPOS_ADMIN_SUBJECTS` | unset | subjects (`<issuer>\|<sub>`) the owner policy lets act on every object |
| `TOPOS_LOCAL_ISSUER_KEY` | unset | a PEM PKCS#8 ECDSA P-256 or RSA (2048 bits or more) private key; set, toposd accepts the tokens `toposd token` signs and serves its key set at `/.well-known/jwks.json` |
| `TOPOS_DB_URL`, `TOPOS_DB_POOL_URL` | unset | Postgres for sessions, and an optional transaction-pooling URL for queries |
| `TOPOS_MODELS_URL` | required | the model connection of an agent that names none: one door of a Lux gateway, such as `https://lux.example/v1/models/anthropic`, or a provider's API |
| `TOPOS_MODELS_KEY` | unset | the credential sent with `TOPOS_MODELS_URL`; a session whose agent names no credential fails its turn with `model_credential_missing` without it |
| `TOPOS_RUNNER_CAPACITY` | `16` | how many hosted sessions the server drives at once; `0` runs none |
| `TOPOS_CELLA_URL`, `TOPOS_CELLA_TOKEN_FILE` | unset | the Cella control plane hosted sessions' sandboxes are created on, and the file holding the bearer presented to it, read on every request; without a URL a hosted session fails its turn with `machine_unavailable` |
| `TOPOS_MACHINE_HELPERS`, `TOPOS_MACHINE_DIR` | `/usr/local/lib/topos`, `/tmp/topos` | where the helper builds a sandbox receives are, and where inside the sandbox they go |

## Roles

| Command | Today |
|---|---|
| `toposd` or `toposd serve` | serves `/livez`, `/readyz` and `/version` on both listeners, the build identity at `/`, and the local issuer's key set when there is one |
| `toposd runner` | exits 1: spec 016 builds it |
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

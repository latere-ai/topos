# Configuration

`toposd` reads its configuration from `TOPOS_*` variables once at start-up.
A start-up with anything missing or malformed exits 1 with one line that
names every problem. Spec [002](../specs/002-scaffold-and-configuration.md)
is the reference for every variable; this page lists the ones read today.

| Variable | Default | Meaning |
|---|---|---|
| `TOPOS_PUBLIC_ADDR` | `:8080` | the listener for the API and the public probes |
| `TOPOS_INTERNAL_ADDR` | `:8081` | the listener for the cluster's probes, metrics and, later, the runners |

## Roles

| Command | Today |
|---|---|
| `toposd` or `toposd serve` | serves `/livez`, `/readyz` and `/version` on both listeners and the build identity at `/` |
| `toposd runner` | exits 1: spec 016 builds it |
| `toposd check` | exits 1: spec 028 builds it |
| `toposd token` | exits 1: spec 006 builds it |
| `toposd -version` | prints the build identity |

Exit codes: 0 on a clean stop, 1 on a start-up or runtime failure, 2 on a
usage error.

# CLI and configuration reference

## Commands

```
mockmint serve    [-config file] [-addr :8080] [-admin-addr 127.0.0.1:9090|off] [-amqp-url amqp://...] [package paths...]
mockmint validate [-config file] [package paths...]
mockmint version
mockmint help
```

Package paths may be package directories, `.zip`/`.tar.gz` archives, or
directories of packages. Paths given as arguments are **added** to
`packages.paths` from the config file and environment.

### `serve`

Loads every package, starts the mock listener and (unless disabled) the admin
listener, and runs until `SIGINT`/`SIGTERM`, then shuts down gracefully
within `http.shutdownTimeout`. The startup log line reports the listen
addresses, package count and startup time.

| flag | overrides | |
|---|---|---|
| `-config` | | YAML server config file |
| `-addr` | `http.addr` | mock traffic listener |
| `-admin-addr` | `admin.addr` | admin listener; `off` disables it |
| `-amqp-url` | `amqp.url` | RabbitMQ URL; empty = HTTP only |

A broken package set at startup is fatal. After that, reloads
(`POST /admin/reload`, `SIGHUP` on Unix, package upload or delete) keep the
previous set on failure. An unreachable broker never blocks startup; the
engine retries in the background.

### `validate`

Loads packages, builds the HTTP routes and plans the RabbitMQ topology
exactly as `serve` would, without listening or connecting. Prints one line
per package:

```
ok  notebook 1.0  mounted at /notebook/1.0  5 operations, 0 warnings
```

Warnings (examples that do not match their schema, overridden example
names, ...) are logged but do not fail validation.

### Exit codes

| code | meaning |
|---|---|
| 0 | success |
| 1 | invalid configuration or package, listen failure, unclean shutdown |
| 2 | usage error: unknown command, bad flag, `validate` without paths |

## Configuration precedence

built-in defaults → config file (`-config`) → `MOCKMINT_*` environment → flags.

Unknown YAML keys and unknown `MOCKMINT_*` variables are errors, so typos
fail fast. The config file format is in the
[README](../README.md#server-configuration).

## Environment variables

A variable's name is `MOCKMINT_` plus the YAML path upper-cased and joined
with `_`. Camel-case keys are upper-cased **without** extra underscores
(`http.readHeaderTimeout` → `MOCKMINT_HTTP_READHEADERTIMEOUT`). Durations
use Go syntax (`500ms`, `30s`); lists are comma-separated.

| variable | default | |
|---|---|---|
| `MOCKMINT_HTTP_ADDR` | `:8080` | mock listener |
| `MOCKMINT_HTTP_READHEADERTIMEOUT` | `5s` | |
| `MOCKMINT_HTTP_IDLETIMEOUT` | `60s` | |
| `MOCKMINT_HTTP_SHUTDOWNTIMEOUT` | `10s` | graceful shutdown budget |
| `MOCKMINT_HTTP_MAXBODYBYTES` | `10485760` | larger bodies get `413` |
| `MOCKMINT_ADMIN_ADDR` | `127.0.0.1:9090` | empty disables; the Docker image sets `:9090` |
| `MOCKMINT_ADMIN_TOKEN` | | bearer token for `/admin/*` |
| `MOCKMINT_ADMIN_DATADIR` | | persist uploaded packages here |
| `MOCKMINT_ADMIN_MAXUPLOADBYTES` | `67108864` | |
| `MOCKMINT_ADMIN_TRAFFICSIZE` | `200` | exchanges kept in the traffic buffer |
| `MOCKMINT_ADMIN_REDACTHEADERS` | | extra headers to redact |
| `MOCKMINT_ADMIN_RECORDLIMIT` | `100` | proxied recordings kept per package |
| `MOCKMINT_AMQP_URL` | | `amqp://` or `amqps://`; empty = HTTP only |
| `MOCKMINT_AMQP_PREFETCH` | `10` | 1–65535 |
| `MOCKMINT_AMQP_HEARTBEAT` | `10s` | |
| `MOCKMINT_AMQP_RECONNECTMIN` | `500ms` | backoff floor |
| `MOCKMINT_AMQP_RECONNECTMAX` | `30s` | backoff ceiling |
| `MOCKMINT_AMQP_CONFIRMTIMEOUT` | `5s` | publisher confirm wait |
| `MOCKMINT_LOG_LEVEL` | `info` | `debug` logs every request |
| `MOCKMINT_LOG_FORMAT` | `json` | or `text` |
| `MOCKMINT_PACKAGES_PATHS` | | package paths |
| `MOCKMINT_DEFAULTS_VALIDATION` | `warn` | `strict`, `warn` or `off` |
| `MOCKMINT_DEFAULTS_SEED` | `0` | `0` = random |

Validation rules: `admin.addr` must differ from `http.addr`; `amqp.reconnectMin`
must be positive and not above `reconnectMax`; sizes and limits must be
positive.

## Response headers

| header | when |
|---|---|
| `X-Mockmint-Example` | name of the example that answered |
| `X-Mockmint-Validation: failed` | `warn` mode served a request that failed validation |
| `Allow` | on `405` |
| `Retry-After` | on `429` from a rate limit |

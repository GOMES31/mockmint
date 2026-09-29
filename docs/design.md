# mockmint design (Phase 1: HTTP core)

## OpenAPI library choice

Evaluated with an identical probe (load a 3.1 spec, validate it, validate three
request bodies, read a 3.1 `type: [object, "null"]` schema), built with
`-trimpath -ldflags="-s -w"` on Go 1.27:

| | kin-openapi v0.149 | libopenapi v0.41 + validator v0.14 |
|---|---|---|
| Probe binary | 9.3 MB | 15.3 MB |
| Transitive packages | 42 | 102 |
| 3.1 type arrays | yes | yes |
| Request validation | yes | yes |
| Spec validation | rejected an operation missing a path parameter | accepted it |

**Decision: `getkin/kin-openapi`.** It is smaller (priority 2), stricter about
spec correctness (priority 1), and its `openapi3filter` validator accepts a
route we construct ourselves, so routing stays on `net/http`. Its gorilla/mux
router is not used.

## Package boundaries

```
cmd/mockmint            flags, config, wiring, signal handling
internal/config         server config: YAML file + MOCKMINT_* env overrides
internal/pkg            mock package model + loader (dir, .zip, .tar.gz via fs.FS)
internal/spec/openapi   spec → []Operation, examples, schema example synthesis, validator
internal/dispatch       Dispatcher interface + strategies, rule matchers, mini JSONPath
internal/template       text/template engine, helpers, deterministic RNG and clock
internal/behavior       latency, faults, rate limiting
internal/protocol/http  router (net/http ServeMux), request pipeline, response writer
internal/problem        RFC 9457 problem+json
```

Dependencies point downward only: `protocol/http` → `pkg` → `spec/openapi`,
`dispatch`, `template`, `behavior` → `problem`. Nothing imports `protocol/http`.

## Request pipeline

```
ServeMux (one handler per path template, method-agnostic)
  → method lookup            (405 problem + Allow header)
  → rate limit               (429 problem + Retry-After)
  → read body (bounded)      (413 problem)
  → validate (strict/warn/off; 400/415 problem in strict)
  → dispatch → example name  (fallback → 404 "no matching example")
  → faults (status / drop)
  → latency (ctx-aware)
  → render template → write
unmatched path → 404 problem
```

Path templates are registered once per path (not per method) so mockmint, not
ServeMux, owns 404/405 and can answer with problem+json. OpenAPI parameter
names are not always valid ServeMux wildcard names (`pet-id`), so wildcards are
renamed `p0..pN` and mapped back. Segments that mix literals and parameters
(`/files/{name}.json`) register a full-segment wildcard and are checked with a
compiled regexp in the handler. ServeMux pattern conflicts (it panics) are
recovered and reported as load errors naming both operations.

The active router is held in an `atomic.Pointer`, so Phase 3 hot reload swaps
a fully built router with no locks on the request path.

## Examples

An `Example` is a named request/response pair. Sources, in order (later names
override earlier ones, with a warning):

1. Spec: response `examples`/`example` per status and media type; parameter
   and request body examples with the same name form the request half
   (Microcks convention).
2. Standalone files: `examples/*.yaml|json` in the package.
3. Synthesis: an operation with no examples gets one generated from the
   response schema (`generated`).

Examples are checked against their schema at load time; mismatches are logged
as warnings.

## Dispatchers

```go
type Dispatcher interface {
    Dispatch(r *Request) (example string, ok bool, err error)
}
```

`static`, `path_params`, `query_params`, `header`, `body_jsonpath`,
`sequence`, `script` (expr-lang, compiled at load, node-count limited, no I/O),
plus `auto` (default): pick the example whose request half matches best, else
the default example. Rules are evaluated in declaration order; first match
wins. `sequence` uses an atomic counter owned by the operation, so it resets on
reload.

In `auto` mode the implicit default answers unmatched requests only when it
has no request half itself. Otherwise `GET /notes/77` would receive the example
recorded for `/notes/2`, which is wrong; such requests fall through to the
fallback (or the 404 problem). An explicitly configured `default` is always
honored. The default example prefers 2xx examples without a request half.

## Resource limits

Request bodies are capped (`http.maxBodyBytes`, 413). Archives are capped at
10k files and 64 MiB uncompressed, with entry names that escape the root
rejected. Schema synthesis caps generated arrays at 100 items, strings at 10k
characters, and a whole generated value at 10k nodes (nested arrays multiply
per-array caps); over-budget schemas fail the load asking for an example.
Directory packages are read through `os.Root`, so symlinks cannot escape the
package; templated headers render in sorted order so seeded output is stable. Template output is capped at 4 MiB per render.

## Measured (Phase 1)

Windows 11, Ryzen 5 5600, Go 1.27, `-trimpath -ldflags="-s -w"`:

| | target | measured |
|---|---|---|
| Binary (linux/amd64, static) | < 30 MB | 13.1 MB (arm64: 12.1 MB) |
| Startup to listening (original HTTP sample) | < 200 ms | 19–24 ms in-process |
| Idle memory | < 30 MB RSS | 16.6 MB working set; 18.7 MB after 2k requests |
| Static GET, in-process | — | ~5.9 µs, 55 allocs |
| Templated, strictly validated POST | — | ~22 µs, 143 allocs |

Linux RSS inside the distroless image is still to be confirmed where Docker
is available.

After Phase 3 (admin listener, both example packages, Windows): idle
working set 17.0–17.3 MB over three runs, peak under 20 MB after 1,500
requests; startup 29 ms; linux/amd64 binary 14.2 MB.

## Determinism

Each request gets a `rand.Rand` seeded from `hash(seed, operation, canonical
request)` when a package seed is set; the same request always renders the same
body and the same latency/fault decisions. `clock` freezes `now`. With no seed,
the RNG is seeded from crypto/rand per request.

# Phase 2: RabbitMQ engine

## AsyncAPI model

`internal/spec/asyncapi` decodes AsyncAPI 2.6 and 3.0 documents into typed Go
structs (`yaml.v3`), resolving `$ref`s at the `yaml.Node` level so that local
pointers (`#/components/...`) and package-relative files both work, with the
same "no escaping the package" rule as OpenAPI. Both versions normalize into
one model:

```
Spec → Channels (address, AMQP binding: exchange/queue)
     → Operations (id, action send|receive, channel, messages, reply)
Message → payload/headers schema (compiled with santhosh-tekuri/jsonschema/v6,
          already a dependency via kin-openapi), named examples, content type
```

Action is always from the **application's** point of view, which is the role
mockmint plays. AsyncAPI 2.x `subscribe` (others subscribe to what the app
sends) becomes `send`; `publish` becomes `receive`.

## Mock behavior per operation

| operation | mockmint does |
|---|---|
| `receive` with a reply (3.0 `reply`, or `replyWith` in mockmint.yaml) | **request/reply**: consume the queue, dispatch to a reply example, publish it to `reply_to` (or the reply channel) with the request's `correlation_id`, then ack |
| `receive` without a reply | consume, validate, ack (a sink; invalid payloads go to the DLX in strict mode) |
| `send` | **publish**: send examples on a `schedule`, or on demand through `Engine.Publish` (the Phase 3 admin API calls this) |

Request and reply examples pair by name (the HTTP convention again): a request
message example named `big` constrains the reply example named `big` for the
`auto` dispatcher. All dispatcher types work unchanged, with AMQP headers as
headers and the routing key as the path.

## Reliability

- One connection; one channel per consumer (own prefetch, manual acks) plus
  one publisher channel in confirm mode.
- A reply is acked only after its publish is **confirmed**, so a broker
  failure mid-reply redelivers the request (at-least-once).
- Outcomes: valid → reply, ack. Invalid payload or headers (strict), no
  matching example, dispatch/template error, a `deadletter` fault, a request
  with no `reply_to` and no fixed reply address, or an **unroutable** reply →
  `nack(requeue=false)`, which the broker routes to the queue's DLX when one
  is configured. Unroutable replies are dead-lettered rather than requeued:
  when the caller's reply queue is gone for good, requeueing would loop
  forever. Transient publish failures (no confirm, lost channel) →
  `nack(requeue=true)`.
- Replies are published `mandatory`. Publish and confirm are serialized on
  the publisher channel so a `basic.return` (sent before its `basic.ack`, and
  handed over by amqp091 in wire order) belongs to the publish in flight.
  This gives up reply pipelining, an acceptable cost for a mock.
- A publisher-channel exception (e.g. a deleted exchange) restarts the
  session, which redeclares the topology. Exchange overrides in mockmint.yaml
  are checked with a passive declare, since mockmint does not own them.
- Connection or channel loss → reconnect with exponential backoff and full
  jitter (`reconnect.min`..`reconnect.max`), then redeclare topology and
  restart consumers and schedules.
- No `amqp.url` → the engine is not started; HTTP runs alone and nothing
  reports unhealthy. A configured but unreachable broker does not block
  startup; the engine keeps retrying in the background.

Message handling is a pure function (`delivery → outcome`) so it is unit
tested without a broker; the connection manager is covered by testcontainers
integration tests (`//go:build integration`).

## Behaviors on messages

`latency` applies before replying. Faults take `action: drop` (ack, never
reply: a lost reply) or `action: deadletter`; `status` faults and `rateLimit`
are HTTP-only and rejected for async operations at load time.

# Phase 3: admin API, state, proxy and hot reload

## Runtime and hot reload

`internal/app` owns the live package set. A reload loads every package
(configured paths plus uploaded archives), builds the HTTP router and plans
the RabbitMQ topology **before** touching anything live. Only when all of that
succeeds does it swap the router (an atomic pointer, as in Phase 1) and
replace the AMQP engine (start the new one, stop the old one: topology
declarations are idempotent, and in-flight messages of the old engine are
acked or requeued). A failed reload changes nothing and reports every error.

Triggers: `POST /admin/reload`, `SIGHUP`, uploading or deleting a package.
There is no file watcher: Kubernetes ConfigMap updates arrive by symlink swap,
and an explicit reload (or a sidecar calling it) is simpler and deterministic.

Uploaded packages are kept in memory and, when `admin.dataDir` is set,
written there so they survive restarts. A package from `packages.paths` cannot
be replaced or deleted through the API (409): the files are the source of
truth.

## State

`internal/state` defines a `Store` interface (namespace = package, then
collection and key) with a bounded in-memory implementation. Values are
stored as JSON bytes, so callers never share mutable values. State survives
reloads of a package with the same name and is cleared through the admin API.

Operations opt in declaratively; no Go code and no template gymnastics for
the common CRUD shape:

```yaml
operations:
  createNote: {state: {action: create, collection: notes, key: "{{.Response.id}}"}}
  getNote:    {state: {action: read,   collection: notes, key: "{{.Request.Params.noteId}}"}}
  updateNote: {state: {action: update, collection: notes, key: "{{.Request.Params.noteId}}"}}
  deleteNote: {state: {action: delete, collection: notes, key: "{{.Request.Params.noteId}}"}}
  listNotes:  {state: {action: list,   collection: notes}}
```

| action | behavior |
|---|---|
| create | render the example as usual, then store the **response body** under `key` (evaluated with `.Response`, the decoded body), so GET returns exactly what POST did |
| read | found → the example's status and headers with the stored value as body; missing → fallback, else 404 problem |
| update | missing → as read; found → store `merge` (shallow merge of the request body, default), `request` or `response`, respond with the stored value |
| delete | missing → as read; found → remove, respond with the example |
| list | the example's status with a JSON array of the collection's values, in key order |

Templates also get `.State.Value` (read/update) and `.State.Get`/`.State.List`.
Stateful operations still validate and dispatch normally; state applies to
the chosen 2xx example (error examples are returned untouched).

## Proxy and recorder

A package can set `proxy: {url, on: [unmatchedRoute, noExample]}`. Requests
under the package base path that match no operation, and/or requests whose
dispatcher finds no example, are forwarded (base path stripped) instead of
getting a 404. Proxied exchanges for known operations are recorded (bounded)
and exported by the admin API **as example files** in mockmint's own format,
so recording against a live service bootstraps a package.

## Admin API

A separate listener (`admin.addr`, default `127.0.0.1:9090`: management is
local unless explicitly exposed, and a non-loopback listener without a token
logs a warning; the Docker image sets `:9090` for probes). `/healthz`, `/readyz`
and `/metrics` are unauthenticated (probes and scrapers); `/admin/*` requires
`Authorization: Bearer <admin.token>` when a token is set (constant-time
compare). Errors are problem+json. The API itself is described in
`docs/admin-openapi.yaml`, which the tests load with mockmint's own OpenAPI
loader.

Readiness: 503 until the first successful load, and while a configured broker
is disconnected for packages that have async operations. No broker
configured is not a failure (Phase 2's graceful degradation).

## Observability

- **Metrics:** a small in-tree Prometheus text-format registry (counters,
  gauges, histograms with fixed label sets) instead of `client_golang`, which
  would add ~2.5 MB and a dependency tree for a few series. Labels are bounded
  by the specs (package, operation, status).
- **Traffic:** a fixed-size ring buffer of recent HTTP and AMQP exchanges.
  Sensitive headers (`Authorization`, `Cookie`, `Set-Cookie`,
  `Proxy-Authorization`, `X-Api-Key`, plus `admin.redactHeaders`) are
  replaced with `[REDACTED]`; bodies are truncated to 4 KiB.

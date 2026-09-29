# mockmint

Lightweight API and RabbitMQ mocking from OpenAPI and AsyncAPI specs and
example files: a single static binary (~14 MB), ~20 ms startup, under 20 MB
resident at idle. No Go code per mock.

> Status: **Phases 1–2 (HTTP core, RabbitMQ engine)**. The admin API, state
> and proxying (Phase 3) and contract testing (Phase 4) are not built yet.

## Quick start

```sh
make build
./bin/mockmint serve examples/notebook

curl -i localhost:8080/notebook/1.0/notes/1          # read the "welcome" note
curl -i 'localhost:8080/notebook/1.0/notes?status=published'  # list published notes
curl -i localhost:8080/notebook/1.0/notes -H 'Content-Type: application/json' \
     -d '{"title":"Meeting","content":"Agenda","status":"draft"}'  # create
curl -i -X PUT localhost:8080/notebook/1.0/notes/1 -H 'Content-Type: application/json' \
     -d '{"title":"Meeting","content":"Updated agenda","status":"published"}'  # update
curl -i -X DELETE localhost:8080/notebook/1.0/notes/1  # delete
```

Docker (distroless, non-root, amd64/arm64):

```sh
make docker                       # serves /examples
docker run -p 8080:8080 -v "$PWD/my-mocks:/mocks:ro" mockmint:dev serve /mocks
```

`mockmint validate <paths...>` loads packages and exits non-zero on errors,
so it works as a CI lint step for mock definitions.

The Notebook example demonstrates CRUD responses. Mockmint does not persist
created, updated, or deleted notes; stateful mocking is planned for Phase 3.

RabbitMQ (see [RabbitMQ mocks](#rabbitmq-mocks-asyncapi)):

```sh
docker run -d -p 5672:5672 rabbitmq:4-alpine
./bin/mockmint serve -amqp-url amqp://guest:guest@localhost:5672/ examples/orders-events
```

## Mock packages

A package is a directory (or `.zip` / `.tar.gz`) containing:

```
openapi.yaml        # OpenAPI 3.0 or 3.1 (detected by its top-level `openapi` key)
asyncapi.yaml       # AsyncAPI 2.6 or 3.0 (detected by `asyncapi`)
mockmint.yaml       # optional: dispatch rules, behaviors, overrides
examples/*.yaml     # optional: extra HTTP examples
```

A package needs at least one of the two specs; it can have both.

`mockmint serve` takes package directories, archives, or directories *of*
packages (for example a Kubernetes ConfigMap mount). Routes are mounted at
`/{name}/{version}` — by default the slugified `info.title` and
`info.version` — or wherever `basePath` says (`/` mounts unprefixed).

External `$ref`s resolve only inside the package; remote and escaping
references are rejected.

### Examples

Named examples come from three places (later ones replace earlier ones with
the same name, with a warning):

1. **The spec.** Response `examples` per status and media type. Parameter and
   request body examples *with the same name* form the example's request half:

   ```yaml
   parameters:
     - name: noteId
       in: path
       examples: {welcome: {value: 1}, ideas: {value: 2}}
   responses:
     "200":
       content:
         application/json:
           examples:
             welcome: {value: {id: 1, title: Welcome}}
             ideas: {value: {id: 2, title: Ideas}}
   ```

   A lone `example` (no name) is named after its status, e.g. `"404"`.

2. **Example files** in `examples/`:

   ```yaml
   operation: createNote           # operationId or "POST /notes"
   examples:
     publish-first:
       request:                    # optional; used by the auto dispatcher
         params: {}                # path parameters
         query: {}
         headers: {}
         body: {status: published} # matched as a subset of the request body
       response:
         status: 422               # default: the lowest declared 2xx
         mediaType: application/problem+json   # default: from the spec
         body: {title: Create a draft before publishing}  # string = verbatim, else JSON
   ```

3. **Synthesis.** If no example covers the lowest 2xx response, one named
   `generated` is built from its schema (formats, enums, bounds, `allOf`,
   `oneOf`, 3.1 type arrays; cycles are cut). It is deterministic.

Examples that do not match their schema are reported as warnings at load time.

**Default example:** one named `default`; otherwise the first (alphabetically)
2xx example without a request half; otherwise the first 2xx example.

### mockmint.yaml

```yaml
name: notebook               # default: slug of info.title
version: "1.0"               # default: info.version
basePath: /api               # default: /{name}/{version}; "/" = unprefixed
spec: openapi.yaml           # default: auto-detected
validation: strict           # strict | warn | off   (default: server default, warn)
seed: 42                     # deterministic templates, latency and faults
clock: 2025-01-01T12:00:00Z  # frozen template time
templating: auto             # auto (bodies containing "{{") | on | off

behavior:                    # package default for every operation
  latency: {min: 5ms, max: 25ms}     # or a fixed "100ms"
  faults:
    - {probability: 0.05, status: 503}
    - {probability: 0.01, action: drop}   # close the connection, no response
  rateLimit: {rps: 50, burst: 100}        # per operation; 429 + Retry-After

fallback: {example: missing}  # or {status: 404, mediaType: ..., headers: ..., body: ...}

operations:                   # keyed by operationId or "METHOD /path"
  getNote:
    dispatcher: {...}
    behavior: {...}           # replaces the package behavior field by field
    fallback: {...}
    validation: off
```

### Dispatchers

| type | selects by | config |
|---|---|---|
| `auto` (default) | best-matching example request half (most matching constraints wins) | — |
| `static` | always one example | `example` |
| `path_params` | path parameters | `rules` |
| `query_params` | query parameters (any value matches) | `rules` |
| `header` | request headers (case-insensitive names) | `rules` |
| `body_jsonpath` | JSON body fields (`$.a.b[0]`, `$['k']`) | `rules` |
| `sequence` | n-th call → n-th example | `examples`, `exhausted: last \| cycle \| fallback` |
| `script` | [expr](https://expr-lang.org) expression returning an example name | `script` |

Rules are tried in order; all conditions of a rule must hold:

```yaml
dispatcher:
  type: body_jsonpath
  rules:
    - when: {"$.kind": cat, "$.age": {regex: "^[0-9]$"}}
      example: kitten
    - when: {"$.vip": {in: ["true"]}, "$.coupon": {exists: false}}
      example: vip
  default: regular        # "-" = no default, use the fallback
```

A condition is a string (exact match) or one of `{equals}`, `{regex}`,
`{in: [...]}`, `{exists: true|false}`.

Scripts see `method`, `path`, `params`, `query`, `queries`, `headers`,
`body` (decoded JSON) and `rawBody`, have no I/O, and cannot call `now()`:

```yaml
dispatcher:
  type: script
  script: 'body?.total > 100 ? "large" : headers["X-Tier"] == "gold" ? "vip" : ""'
```

When nothing matches: the dispatcher's `default`, else the operation's
fallback, else `404 application/problem+json`. In `auto` mode the implicit
default only catches unmatched requests if it has no request half itself, so
`GET /notes/77` never gets the example recorded for `/notes/2`.

### Templates

Bodies and header values containing `{{` are Go `text/template`s:

| | |
|---|---|
| `.Request.Method` `.Request.Path` | |
| `.Request.Params.id` | path parameter (use `index .Request.Params "pet-id"` for dashed names) |
| `.Request.Query.q` / `.Request.Queries.q` | first / all values |
| `index .Request.Headers "X-Trace"` | first value, canonical name |
| `.Request.Body.field` / `.Request.RawBody` | decoded JSON / raw text |
| `.UUID` `.RandInt 1 6` `.RandFloat 0 1` `.RandBool` `.RandString 8` `.RandItem "a" "b"` | random, seedable |
| `.Now` `.Timestamp` `.Unix` `.UnixMilli` | frozen by `clock` |
| `.Fake.Name` `.Email` `.FirstName` `.LastName` `.Username` `.Company` `.City` `.Country` `.Street` `.Zip` `.Phone` `.Word` `(.Fake.Sentence 5)` | fake data |
| `json v` `jsonPath v "$.a"` `upper` `lower` `trim` `default "x" v` `add` `sub` `mul` | functions |

Use `{{json .Request.Body.name}}` to echo values into JSON safely.

**Determinism:** with a `seed`, each request's randomness (template values,
latency, fault rolls) is derived from the seed, the operation and the request
(method, path, sorted query, body). The same request always gets the same
response, regardless of concurrency. Without a seed, values are random.

### Validation and errors

`strict` rejects invalid requests with `400` (listing each violation) and
undeclared content types with `415`; `warn` serves anyway, logs, and sets
`X-Mockmint-Validation: failed`; `off` skips validation. Security
requirements are not enforced.

All mockmint errors are [RFC 9457](https://www.rfc-editor.org/rfc/rfc9457)
`application/problem+json`: 404 (no route / no matching example), 405 (with
`Allow`), 413, 415, 429, 500 and injected faults. Each response names the
example used in `X-Mockmint-Example`.

## RabbitMQ mocks (AsyncAPI)

mockmint plays the application an AsyncAPI document describes. Operations
map to behavior by their `action` (AsyncAPI 2.x `publish` is `receive`,
`subscribe` is `send`):

| operation | mockmint |
|---|---|
| `receive` with a reply (3.0 `reply`, or `replyWith`) | **request/reply**: consumes the queue, picks a reply example, publishes it to the request's `reply_to` (or the reply channel's address) with the request's `correlation_id` (falling back to its `message_id`), and acks only after the broker confirms the reply |
| `receive` without a reply | **sink**: validates and acks |
| `send` | **publish**: on a `schedule`, or on demand (admin API in Phase 3) |

Queues and exchanges come from the AMQP channel bindings: `is: queue` names a
queue; `is: routingKey` publishes to the binding's exchange with the channel
address as routing key, and receive operations on it get a queue bound to
that exchange (the binding's queue name, else `mockmint.<package>.<operation>`,
auto-deleted). Exchanges and queues default to durable; exchanges to `topic`.

Request and reply examples **pair by name**, like HTTP: a request example
`bulk` selects the reply example `bulk`; a reply example without a paired
request example is the catch-all. Every dispatcher works on messages: AMQP
headers are headers, the routing key is the path, and the body is the
payload. Templates see `.Request.Body`, `.Request.Headers` (AMQP headers plus
`Correlation-Id`, `Reply-To`, `Message-Id`, `Routing-Key`, `Exchange`,
`Content-Type`) and all the usual helpers. Messages without examples get one
synthesized from the payload schema.

```yaml
async:
  declare: true              # declare exchanges, queues, bindings on connect
  validation: strict         # default: the package validation
  deadLetter:
    exchange: orders.dlx     # fanout exchange + orders.dlx.queue, set as the
                             # x-dead-letter-exchange of every consumed queue
  behavior:                  # default for every async operation
    latency: {min: 5ms, max: 20ms}
    faults:
      - {probability: 0.01, action: drop}        # ack but never reply
      - {probability: 0.01, action: deadletter}  # reject to the DLX
  operations:                # keyed by AsyncAPI operation id
    placeOrder:
      queue: orders.requests # override the consumed queue
      replyWith: orderReply  # 2.x: the send operation whose messages are replies
      noReply: false         # true turns a 3.0 request/reply into a sink
      dispatcher: {...}      # any dispatcher
      fallback: accepted     # reply example when nothing matches
    publishOrderCreated:
      exchange: orders.events   # override the publish target
      routingKey: order.created
      schedule: {interval: 5s, initialDelay: 1s, examples: [widget, gadget]}
```

**Outcomes.** A valid message gets its reply and is acked. In `strict` mode an
invalid payload, a reply example that fails its schema, no matching example
without a fallback, or a `deadletter` fault **rejects** the message without
requeue, so the broker routes it to the DLX. A reply the broker does not
confirm (or a lost connection) **requeues** the request. `warn` logs
validation problems and carries on.

**Reliability.** One connection with a channel per consumer (`prefetch`,
manual acks) and a confirm-mode publisher channel. When the connection or a
channel drops, mockmint reconnects with exponential backoff and full jitter,
redeclares the topology, and resumes consumers and schedules. Without
`amqp.url` the engine is off and HTTP runs alone; a configured broker that is
down never blocks startup.

## Server configuration

`mockmint serve -config mockmint-server.yaml`, and/or `MOCKMINT_*`
environment variables (`http.addr` → `MOCKMINT_HTTP_ADDR`, lists
comma-separated). Unknown keys and variables are errors.

```yaml
http:
  addr: ":8080"
  readHeaderTimeout: 5s
  idleTimeout: 60s
  shutdownTimeout: 10s
  maxBodyBytes: 10485760
amqp:
  url: ""           # amqp://user:pass@host:5672/vhost; empty = HTTP only
  prefetch: 10      # unacked messages per consumer
  heartbeat: 10s
  reconnectMin: 500ms
  reconnectMax: 30s
  confirmTimeout: 5s
log:
  level: info       # debug logs every request
  format: json      # or text
packages:
  paths: [/mocks]
defaults:
  validation: warn
  seed: 0           # 0 = random
```

## Development

```sh
make test         # unit tests
make race         # needs a C compiler (cgo) for the race detector
make integration  # RabbitMQ tests via testcontainers (Docker, or Podman with
                  # TESTCONTAINERS_RYUK_DISABLED=true)
make lint         # golangci-lint v2
make bench
```

See [docs/design.md](docs/design.md) for architecture and decisions.

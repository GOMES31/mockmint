# mockmint

Lightweight API mocking from OpenAPI specs and example files: a single static
binary (~13 MB), ~20 ms startup, under 20 MB resident at idle. No Go code per
mock.

> Status: **Phase 1 (HTTP core)**. RabbitMQ/AsyncAPI (Phase 2), the admin API,
> state and proxying (Phase 3) and contract testing (Phase 4) are not built yet.

## Quick start

```sh
make build
./bin/mockmint serve examples/petstore

curl -i localhost:8080/petstore/1.0/pets/1          # example "tom"
curl -i localhost:8080/petstore/1.0/pets/9          # fallback → 404 example
curl -i 'localhost:8080/petstore/1.0/pets?status=sold'
curl -i localhost:8080/petstore/1.0/pets -H 'Content-Type: application/json' \
     -d '{"name":"Kiwi","kind":"cat"}'              # templated 201
```

Docker (distroless, non-root, amd64/arm64):

```sh
make docker                       # serves /examples
docker run -p 8080:8080 -v "$PWD/my-mocks:/mocks:ro" mockmint:dev serve /mocks
```

`mockmint validate <paths...>` loads packages and exits non-zero on errors,
so it works as a CI lint step for mock definitions.

## Mock packages

A package is a directory (or `.zip` / `.tar.gz`) containing:

```
openapi.yaml        # OpenAPI 3.0 or 3.1 (detected by its top-level `openapi` key)
mockmint.yaml       # optional: dispatch rules, behaviors, overrides
examples/*.yaml     # optional: extra examples
```

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
     - name: petId
       in: path
       examples: {tom: {value: 1}, rex: {value: 2}}
   responses:
     "200":
       content:
         application/json:
           examples:
             tom: {value: {id: 1, name: Tom}}
             rex: {value: {id: 2, name: Rex}}
   ```

   A lone `example` (no name) is named after its status, e.g. `"404"`.

2. **Example files** in `examples/`:

   ```yaml
   operation: createPet            # operationId or "POST /pets"
   examples:
     no-birds:
       request:                    # optional; used by the auto dispatcher
         params: {}                # path parameters
         query: {}
         headers: {}
         body: {kind: bird}        # matched as a subset of the request body
       response:
         status: 422               # default: the lowest declared 2xx
         mediaType: application/problem+json   # default: from the spec
         headers: {X-Reason: birds}
         body: {title: Birds are not supported}  # string = verbatim, else JSON
   ```

3. **Synthesis.** If no example covers the lowest 2xx response, one named
   `generated` is built from its schema (formats, enums, bounds, `allOf`,
   `oneOf`, 3.1 type arrays; cycles are cut). It is deterministic.

Examples that do not match their schema are reported as warnings at load time.

**Default example:** one named `default`; otherwise the first (alphabetically)
2xx example without a request half; otherwise the first 2xx example.

### mockmint.yaml

```yaml
name: petstore               # default: slug of info.title
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
  getPet:
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
`GET /pets/77` never gets the example recorded for `/pets/2`.

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
make lint         # golangci-lint v2
make bench
```

See [docs/design.md](docs/design.md) for architecture and decisions.

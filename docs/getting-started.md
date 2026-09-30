# Getting started: your first mock package

This walks through mocking a small Inventory API from scratch: a spec alone,
then examples, strict validation, templates and state. It takes about ten
minutes. The [README](../README.md) is the full reference for every option
used here.

## 1. Build mockmint

```sh
make build          # ./bin/mockmint (bin/mockmint.exe on Windows)
./bin/mockmint version
```

Or use the container image: `make docker`, then replace `./bin/mockmint`
below with `docker run --rm -p 8080:8080 -p 9090:9090 -v "$PWD/inventory:/mocks/inventory:ro" mockmint:dev`
followed by the same arguments (with `/mocks/inventory` as the path).

## 2. A spec is already a mock

Create `inventory/openapi.yaml`:

```yaml
openapi: 3.1.0
info:
  title: Inventory
  version: "1.0"
paths:
  /items/{sku}:
    get:
      operationId: getItem
      parameters:
        - name: sku
          in: path
          required: true
          schema: {type: string}
          examples:
            hammer: {value: HAM-1}
      responses:
        "200":
          description: The item
          content:
            application/json:
              schema: {$ref: "#/components/schemas/Item"}
              examples:
                hammer: {value: {sku: HAM-1, name: Hammer, stock: 12}}
        "404":
          description: Unknown SKU
          content:
            application/problem+json:
              example: {title: Item not found, status: 404}
  /items:
    post:
      operationId: createItem
      requestBody:
        required: true
        content:
          application/json:
            schema: {$ref: "#/components/schemas/Item"}
      responses:
        "201":
          description: Created
          content:
            application/json:
              schema: {$ref: "#/components/schemas/Item"}
components:
  schemas:
    Item:
      type: object
      required: [sku, name, stock]
      properties:
        sku: {type: string}
        name: {type: string}
        stock: {type: integer, minimum: 0}
```

Check it, then serve it:

```sh
./bin/mockmint validate inventory
# ok  inventory 1.0  mounted at /inventory/1.0  2 operations, 0 warnings

./bin/mockmint serve inventory
```

The package is mounted at `/{slug of info.title}/{info.version}`:

```sh
curl -i localhost:8080/inventory/1.0/items/HAM-1
# 200, X-Mockmint-Example: hammer
# {"name":"Hammer","sku":"HAM-1","stock":12}

curl -i localhost:8080/inventory/1.0/items/NOPE
# 404 application/problem+json: "no example of GET /items/{sku} matches this request"

curl -i localhost:8080/inventory/1.0/items -H 'Content-Type: application/json' \
     -d '{"sku":"SAW-2","name":"Saw","stock":3}'
# 201, X-Mockmint-Example: generated  (synthesized from the Item schema)
```

What happened:

- The `hammer` parameter example and the `hammer` response example share a
  name, so they form one request/response pair. The default `auto`
  dispatcher serves it only when the request matches (`sku` is `HAM-1`).
- `createItem` has no examples, so mockmint synthesized one named
  `generated` from the response schema.
- The server default validation is `warn`: an invalid body is still served,
  with `X-Mockmint-Validation: failed` and a log line.

## 3. Make it behave

Add `inventory/examples/create-item.yaml`, an example that echoes the request
into the response with a template:

```yaml
operation: createItem
examples:
  created:
    response:
      status: 201
      body: |
        {"sku": {{json .Request.Body.sku}}, "name": {{json .Request.Body.name}},
         "stock": {{json .Request.Body.stock}}, "id": "{{.UUID}}"}
```

And `inventory/mockmint.yaml`:

```yaml
validation: strict   # reject requests that break the spec
seed: 7              # same request -> same UUIDs, latency and fault rolls

initialState:        # seeded while the collection is empty
  items:
    HAM-1: {sku: HAM-1, name: Hammer, stock: 12}

operations:
  getItem:
    fallback: {example: "404"}   # the spec's unnamed 404 example is named "404"
    state: {action: read, collection: items, key: "{{.Request.Params.sku}}"}
  createItem:
    state: {action: create, collection: items, key: "{{.Response.sku}}"}
```

Restart `serve` (or keep it running and call `POST /admin/reload`, see
below), then:

```sh
# strict validation: 400 problem listing each violation
curl -i localhost:8080/inventory/1.0/items -H 'Content-Type: application/json' -d '{"sku":"SAW-2"}'

# create: the template echoes the body; the response is stored under its sku
curl -i localhost:8080/inventory/1.0/items -H 'Content-Type: application/json' \
     -d '{"sku":"SAW-2","name":"Saw","stock":3}'

# read it back: exactly what POST returned
curl -i localhost:8080/inventory/1.0/items/SAW-2

# unknown key: the fallback example
curl -i localhost:8080/inventory/1.0/items/NOPE
# 404 {"status":404,"title":"Item not found"}
```

## 4. Look inside with the admin API

The admin API listens on `127.0.0.1:9090` by default:

```sh
curl localhost:9090/readyz                              # {"status":"ready"}
curl localhost:9090/admin/packages                      # what is loaded
curl localhost:9090/admin/state/inventory               # stored items
curl -X DELETE localhost:9090/admin/state/inventory     # reset, re-seed initialState
curl 'localhost:9090/admin/traffic?limit=5'             # recent requests, redacted
curl -X POST localhost:9090/admin/reload                # pick up edited files
```

A reload is atomic: if an edited file is broken, it answers `422` with every
error and the previous version keeps serving.

## 5. Use it in tests

- Run `mockmint validate` in CI so a broken package fails the build
  ([CI guide](ci.md)).
- Set a `seed` (and a `clock`, if responses contain times) so snapshots of
  mock responses stay stable.
- Reset state between test cases with `DELETE /admin/state/{package}`.

## Where next

- More dispatchers (`header`, `body_jsonpath`, `sequence`, `script`),
  latency, faults and rate limits: [README](../README.md#dispatchers).
- RabbitMQ request/reply and publishing from AsyncAPI:
  [README](../README.md#rabbitmq-mocks-asyncapi) and `examples/orders-events`.
- Recording a real service into a package: [proxy and recording](../README.md#proxy-and-recording).
- Deploying to a shared namespace: [Kubernetes guide](kubernetes.md).

# Using mockmint in CI/CD

Two jobs for mockmint in a pipeline: **lint** mock packages so a broken mock
fails the build, and **serve** mocks while integration tests run.

## Lint packages

`mockmint validate` loads packages exactly as `serve` does and exits `1` on
any error:

```sh
mockmint validate mocks/          # a directory of packages
```

## Serve mocks during tests

There is no published image yet, so build one from this repository (the build
is cached by Docker layers):

```sh
docker build -t mockmint:ci https://github.com/GOMES31/mockmint.git
```

Start it detached, wait for readiness, run the tests, and dump recent traffic
if they fail:

```sh
docker run -d --name mockmint -p 8080:8080 -p 9090:9090 \
  -v "$PWD/mocks:/mocks:ro" -e MOCKMINT_DEFAULTS_SEED=1 \
  mockmint:ci serve /mocks

curl -fsS --retry 30 --retry-all-errors --retry-delay 1 localhost:9090/readyz

npm test   # or go test, pytest, ... pointed at http://localhost:8080/<package>/<version>

curl -s 'localhost:9090/admin/traffic?limit=50'   # on failure: what the mocks saw
```

Tips:

- A fixed `seed` (per package or `MOCKMINT_DEFAULTS_SEED`) and `clock` make
  responses reproducible across runs.
- Reset state between suites: `curl -X DELETE localhost:9090/admin/state/<package>`.
- `MOCKMINT_DEFAULTS_VALIDATION=strict` turns requests that break the spec
  into `400` problems, so client bugs surface as test failures.

## GitHub Actions

```yaml
name: integration
on: [push, pull_request]
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - name: Build mockmint
        run: docker build -t mockmint:ci https://github.com/GOMES31/mockmint.git

      - name: Validate mocks
        run: docker run --rm -v "$PWD/mocks:/mocks:ro" mockmint:ci validate /mocks

      - name: Start mocks
        run: |
          docker run -d --name mockmint -p 8080:8080 -p 9090:9090 \
            -v "$PWD/mocks:/mocks:ro" -e MOCKMINT_DEFAULTS_SEED=1 mockmint:ci serve /mocks
          curl -fsS --retry 30 --retry-all-errors --retry-delay 1 localhost:9090/readyz

      - name: Test
        run: make test-integration
        env:
          API_BASE_URL: http://localhost:8080/notebook/1.0

      - name: Mock traffic
        if: failure()
        run: |
          curl -s 'localhost:9090/admin/traffic?limit=50'
          docker logs mockmint
```

A `services:` container cannot be used here: services start before checkout
(so the mocks are not there to mount) and cannot override the command.

## RabbitMQ in CI

Start a broker next to mockmint and point it there; mockmint waits for the
broker in the background and `/readyz` turns ready once it is connected:

```sh
docker network create ci
docker run -d --name rabbitmq --network ci rabbitmq:4-alpine
docker run -d --name mockmint --network ci -p 8080:8080 -p 9090:9090 \
  -v "$PWD/mocks:/mocks:ro" -e MOCKMINT_AMQP_URL=amqp://guest:guest@rabbitmq:5672/ \
  mockmint:ci serve /mocks
curl -fsS --retry 60 --retry-all-errors --retry-delay 1 localhost:9090/readyz
```

## Docker Compose (local development)

```yaml
services:
  rabbitmq:
    image: rabbitmq:4-management-alpine
    ports: ["5672:5672", "15672:15672"]
  mockmint:
    build: https://github.com/GOMES31/mockmint.git
    command: [serve, /mocks]
    environment:
      MOCKMINT_AMQP_URL: amqp://guest:guest@rabbitmq:5672/
      MOCKMINT_LOG_FORMAT: text
    volumes: ["./mocks:/mocks:ro"]
    ports: ["8080:8080", "9090:9090"]
```

Edit a package, then `curl -X POST localhost:9090/admin/reload`.

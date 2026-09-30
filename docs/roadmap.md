# Roadmap and implementation status

Status against the original four-phase plan, as of 2026-09-30.

## Done

| phase | scope | status |
|---|---|---|
| 1. HTTP core | package loader (dir, zip, tar.gz), OpenAPI 3.0/3.1 (kin-openapi), `net/http` routing, all dispatchers, templates, latency/faults/rate limits, fallback, RFC 9457 problems, `strict`/`warn`/`off` validation, Dockerfile | complete |
| 2. RabbitMQ engine | AsyncAPI 2.6/3.0, topology declaration, request/reply, sink, scheduled publish, confirms, manual acks, prefetch, DLX, reconnect with backoff, payload validation, HTTP-only degradation, testcontainers tests | complete |
| 3. Admin, state, advanced | admin API with bearer auth, upload/delete, atomic hot reload, state store, proxy and recorder, traffic ring buffer with redaction, on-demand publish, `/healthz` `/readyz` `/metrics` | complete |

Acceptance checks on the current `main`: `go vet ./...` clean,
`golangci-lint run` 0 issues, `go test -race ./...` passes. Measured on
Windows: 14.2 MB linux/amd64 binary, ~29 ms startup, ~17 MB idle working set
(targets: < 30 MB, < 200 ms, < 30 MB).

## Not built yet

### Phase 4: contract testing and CLI tooling

- **`mockmint test` command.** Run a package's examples as a contract test
  suite against a live implementation: send each example's request half to
  a target base URL (or publish it to RabbitMQ) and check the response
  against the example and the spec's schema.
- **`internal/contracttest`.** The runner engine behind it (the plan's
  layout reserves this package).
- **Reports.** JUnit XML (for CI test tabs) and JSON output, with a
  non-zero exit code on failures.
- **Final review and benchmark verification.** Re-measure the targets at
  the end, including **Linux RSS inside the distroless image**, which
  `design.md` still lists as unconfirmed (only Windows working-set numbers
  exist).

### Other gaps against the plan

| item | plan | now |
|---|---|---|
| `web/` | vendored Swagger UI and AsyncAPI viewer | missing |
| Tracing | observability: "metrics, tracing, logging, health" | logs, metrics and health only; no trace propagation or spans |
| External state backends | `state/`: "in-memory and external state backends" | in-memory only; the `Store` interface is ready for another backend |
| Example packages | `petstore`, `orders-events` | `notebook` (replaces petstore deliberately), `orders-events` |

### Housekeeping

- **Module path.** `go.mod` declares `github.com/mockmint/mockmint`, but the
  repository is `github.com/GOMES31/mockmint`, so `go install` from GitHub
  does not work. Either change the module path or host it at the declared
  path.
- **Published image and CI.** No container image is published and the
  repository has no CI workflow; `make lint test race` and a multi-arch
  image push would be natural first jobs.
- **Integration tests** (`make integration`) need Docker or Podman and are
  not part of `make test`; run them before a release.

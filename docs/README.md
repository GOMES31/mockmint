# mockmint documentation

| | |
|---|---|
| [Getting started](getting-started.md) | build a mock package from scratch: spec, examples, validation, templates, state |
| [Reference](../README.md) | package format, `mockmint.yaml`, dispatchers, templates, RabbitMQ, state, proxy, admin API, server config |
| [CLI and configuration](cli.md) | commands, flags, exit codes, every `MOCKMINT_*` variable |
| [Kubernetes](kubernetes.md) | Deployment, ConfigMap packages, probes, reloads, security |
| [CI/CD](ci.md) | linting packages, serving mocks during tests, GitHub Actions, Compose |
| [Admin API spec](admin-openapi.yaml) | OpenAPI 3.1 description of the admin API |
| [Design](design.md) | architecture, decisions and measurements per phase |
| [Roadmap](roadmap.md) | what is built and what is still missing |

Sample packages live in [`examples/`](../examples): `notebook` (stateful
HTTP CRUD) and `orders-events` (RabbitMQ request/reply and publishing).

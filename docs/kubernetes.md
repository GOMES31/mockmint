# Running mockmint on Kubernetes

mockmint fits a shared test namespace as one small Deployment: packages come
from ConfigMaps (or are uploaded through the admin API), probes and
Prometheus use the admin port, and RabbitMQ is optional.

There is no published image yet; build and push your own:

```sh
docker buildx build --platform linux/amd64,linux/arm64 --push \
  -t registry.example.com/mockmint:0.1.0 .
```

(`make docker-multiarch IMAGE=...` runs the same build without `--push`.)

The image is distroless, runs as uid `65532`, listens on `8080` (mocks) and
`9090` (admin), and serves `/examples` unless given other arguments.

## Packages as ConfigMaps

ConfigMap volumes are flat, so use one ConfigMap per package and map the
example files into `examples/` with `items`:

```sh
kubectl create configmap mock-notebook \
  --from-file=examples/notebook/openapi.yaml \
  --from-file=examples/notebook/mockmint.yaml \
  --from-file=examples/notebook/examples/create-note.yaml \
  --from-file=examples/notebook/examples/update-note.yaml
```

Or bundle a package as an archive (a `.tar.gz` in a ConfigMap's
`binaryData`, up to ConfigMap's 1 MiB limit) and mount the single file.

## Deployment

```yaml
apiVersion: v1
kind: Secret
metadata: {name: mockmint}
stringData:
  admin-token: change-me
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: mockmint}
spec:
  replicas: 1                      # state, traffic and recordings are per pod
  selector: {matchLabels: {app: mockmint}}
  template:
    metadata:
      labels: {app: mockmint}
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: "9090"
        prometheus.io/path: /metrics
    spec:
      securityContext:
        runAsNonRoot: true
        seccompProfile: {type: RuntimeDefault}
      containers:
        - name: mockmint
          image: registry.example.com/mockmint:0.1.0
          args: [serve, /mocks]      # a directory of packages
          env:
            - {name: MOCKMINT_LOG_FORMAT, value: json}
            - {name: MOCKMINT_DEFAULTS_VALIDATION, value: strict}
            - name: MOCKMINT_ADMIN_TOKEN
              valueFrom: {secretKeyRef: {name: mockmint, key: admin-token}}
            # - {name: MOCKMINT_AMQP_URL, valueFrom: {secretKeyRef: {name: rabbitmq, key: url}}}
            # - {name: MOCKMINT_ADMIN_DATADIR, value: /data}   # keep uploaded packages
          ports:
            - {name: http, containerPort: 8080}
            - {name: admin, containerPort: 9090}
          livenessProbe:
            httpGet: {path: /healthz, port: admin}
          readinessProbe:
            httpGet: {path: /readyz, port: admin}
            periodSeconds: 5
          resources:
            requests: {cpu: 10m, memory: 32Mi}
            limits: {memory: 128Mi}
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: {drop: [ALL]}
          volumeMounts:
            - {name: notebook, mountPath: /mocks/notebook, readOnly: true}
      volumes:
        - name: notebook
          configMap:
            name: mock-notebook
            items:
              - {key: openapi.yaml, path: openapi.yaml}
              - {key: mockmint.yaml, path: mockmint.yaml}
              - {key: create-note.yaml, path: examples/create-note.yaml}
              - {key: update-note.yaml, path: examples/update-note.yaml}
---
apiVersion: v1
kind: Service
metadata: {name: mockmint}
spec:
  selector: {app: mockmint}
  ports:
    - {name: http, port: 80, targetPort: http}
    - {name: admin, port: 9090, targetPort: admin}
```

Services in the namespace then call
`http://mockmint/notebook/1.0/notes/1`. Set `basePath: /` in a package's
`mockmint.yaml` to mount it unprefixed, e.g. when mockmint stands in for a
single service behind its usual hostname.

Readiness is `503` until packages load and, for packages with AMQP
operations, while a configured broker is disconnected. Without
`MOCKMINT_AMQP_URL`, a missing broker never fails a probe.

## Updating packages

mockmint does not watch files. After a ConfigMap change has propagated to the
pod (up to about a minute), trigger a reload:

```sh
kubectl port-forward deploy/mockmint 9090:9090 &
curl -X POST -H "Authorization: Bearer $TOKEN" localhost:9090/admin/reload
```

A broken package returns `422` with every error, and the old set keeps
serving. Alternatives: `kubectl rollout restart deploy/mockmint` (loses
in-memory state), or upload packages at runtime instead of mounting them:

```sh
tar -czf notebook.tar.gz -C examples/notebook .
curl -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/gzip' \
     --data-binary @notebook.tar.gz localhost:9090/admin/packages/notebook
```

Uploaded packages live in memory; set `MOCKMINT_ADMIN_DATADIR` to a
PersistentVolume (or an `emptyDir` to survive container restarts) to keep
them. Packages mounted from `packages.paths` cannot be replaced through the
API (`409`).

## Security notes

- Always set `MOCKMINT_ADMIN_TOKEN` when the admin port is reachable: the
  admin API can replace every mock. mockmint logs a warning when a
  non-loopback admin listener has no token.
- Do not route the admin port through a public Ingress; expose only `http`.
- `/healthz`, `/readyz` and `/metrics` are unauthenticated by design.
- The traffic buffer redacts `Authorization`, `Cookie`, `Set-Cookie`,
  `Proxy-Authorization` and `X-Api-Key`; add others with
  `MOCKMINT_ADMIN_REDACTHEADERS`.

## Scaling

State, the traffic buffer, recordings and `sequence` counters are per pod
and in memory. Keep `replicas: 1` for stateful or sequence-based mocks; purely
stateless packages scale horizontally.

# syntax=docker/dockerfile:1

# ---- build ----
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/mockmint ./cmd/mockmint

# ---- runtime ----
# distroless/static: CA certificates and tzdata, no shell, runs as uid 65532.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mockmint /mockmint
COPY examples /examples
USER nonroot:nonroot
# 8080: mocks; 9090: admin API. The admin API defaults to loopback; the image
# opens it so probes and scrapers reach /healthz, /readyz and /metrics. Set
# MOCKMINT_ADMIN_TOKEN wherever /admin/* is reachable.
EXPOSE 8080 9090
ENV MOCKMINT_HTTP_ADDR=:8080 MOCKMINT_ADMIN_ADDR=:9090
ENTRYPOINT ["/mockmint"]
CMD ["serve", "/examples"]

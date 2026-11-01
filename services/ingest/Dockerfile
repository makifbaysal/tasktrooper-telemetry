# Build context: repo root (ADR-027; see deploy/docker-compose.yml). Build arg CMD selects the binary.
#   docker build -f services/ingest/Dockerfile --build-arg CMD=ingest-writer .
# services/pkg is copied for the go.mod replace directive (../pkg); the Go SDK comes from proxy.golang.org (ADR-029).
FROM golang:1.26-alpine AS build
ENV CGO_ENABLED=0 GOWORK=off
WORKDIR /src
COPY services/pkg/ services/pkg/
COPY services/ingest/go.mod services/ingest/go.sum services/ingest/
WORKDIR /src/services/ingest
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY services/ingest/ .
ARG CMD=ingest-gw
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${CMD}

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
EXPOSE 4317 4318 4320 9464
ENTRYPOINT ["/app"]

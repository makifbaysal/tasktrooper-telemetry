# Build context: services/ (see deploy/docker-compose.yml). Build arg CMD selects the binary.
#   docker build -f ingest/Dockerfile --build-arg CMD=ingest-writer services/
FROM golang:1.26-alpine AS build
ENV CGO_ENABLED=0 GOWORK=off
WORKDIR /src
COPY pkg/ pkg/
COPY ingest/go.mod ingest/go.sum ingest/
WORKDIR /src/ingest
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY ingest/ .
ARG CMD=ingest-gw
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${CMD}

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
EXPOSE 4317 4318 4320 9464
ENTRYPOINT ["/app"]

# Build context: repo root (ADR-027)  ·  docker build -f services/symbolicator/Dockerfile -t tasktrooper/symbolicator .
# services/pkg is copied for the go.mod replace directive (../pkg); the Go SDK comes from proxy.golang.org (ADR-029).
FROM golang:1.26 AS build
WORKDIR /src
COPY services/pkg/ services/pkg/
COPY services/symbolicator/go.mod services/symbolicator/go.sum services/symbolicator/
WORKDIR /src/services/symbolicator
RUN GOWORK=off go mod download
COPY services/symbolicator/ .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOWORK=off go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/symbolicator ./cmd/symbolicator

# swift-demangle is not available in this image: Swift frames keep their mangled names (frame.mangled).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/symbolicator /symbolicator
ENV TT_SYMBOLICATOR_ADDR=:8095 TT_SYMBOLICATOR_CACHE_DIR=/tmp/tt-symbolicator
EXPOSE 8095
ENTRYPOINT ["/symbolicator"]

# Build context: services/  ·  docker build -f symbolicator/Dockerfile -t tasktrooper/symbolicator services/
FROM golang:1.26 AS build
WORKDIR /src
COPY pkg/ pkg/
COPY symbolicator/go.mod symbolicator/go.sum symbolicator/
WORKDIR /src/symbolicator
RUN GOWORK=off go mod download
COPY symbolicator/ .
RUN CGO_ENABLED=0 GOWORK=off go build -trimpath -ldflags="-s -w" -o /out/symbolicator ./cmd/symbolicator

# swift-demangle is not available in this image: Swift frames keep their mangled names (frame.mangled).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/symbolicator /symbolicator
ENV TT_SYMBOLICATOR_ADDR=:8095 TT_SYMBOLICATOR_CACHE_DIR=/tmp/tt-symbolicator
EXPOSE 8095
ENTRYPOINT ["/symbolicator"]

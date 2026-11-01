# Build context: repo root (ADR-027)  ·  docker build -f services/mcp/Dockerfile -t tasktrooper/mcp .
# services/pkg is copied for the go.mod replace directive (../pkg); the Go SDK comes from proxy.golang.org (ADR-029).
FROM golang:1.26 AS build
WORKDIR /src
COPY services/pkg/ services/pkg/
COPY services/mcp/go.mod services/mcp/go.sum services/mcp/
WORKDIR /src/services/mcp
RUN GOWORK=off go mod download
COPY services/mcp/ .
RUN CGO_ENABLED=0 GOWORK=off go build -trimpath -ldflags="-s -w" -o /out/tt-mcp ./cmd/tt-mcp

# The server speaks MCP over stdin/stdout: no ports, no writes to disk.
# Run it as a sidecar or exec into it from a local agent (ADR-025).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/tt-mcp /tt-mcp
ENTRYPOINT ["/tt-mcp"]

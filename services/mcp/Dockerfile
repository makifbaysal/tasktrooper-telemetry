# Build context: services/  ·  docker build -f mcp/Dockerfile -t tasktrooper/mcp services/
FROM golang:1.26 AS build
WORKDIR /src
COPY pkg/ pkg/
COPY mcp/go.mod mcp/go.sum mcp/
WORKDIR /src/mcp
RUN GOWORK=off go mod download
COPY mcp/ .
RUN CGO_ENABLED=0 GOWORK=off go build -trimpath -ldflags="-s -w" -o /out/tt-mcp ./cmd/tt-mcp

# The server speaks MCP over stdin/stdout: no ports, no writes to disk.
# Run it as a sidecar or exec into it from a local agent (ADR-025).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/tt-mcp /tt-mcp
ENTRYPOINT ["/tt-mcp"]

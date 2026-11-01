# Build context: repo root (ADR-027)  ·  docker build -f services/alerting/Dockerfile --build-arg CMD=alert-engine .
# services/pkg is copied for the go.mod replace directive (../pkg); the Go SDK comes from proxy.golang.org (ADR-029).
FROM golang:1.26 AS build
ARG CMD=alert-engine
WORKDIR /src
COPY services/pkg/ services/pkg/
COPY services/alerting/go.mod services/alerting/go.sum services/alerting/
WORKDIR /src/services/alerting
RUN GOWORK=off go mod download
COPY services/alerting/ .
RUN CGO_ENABLED=0 GOWORK=off go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${CMD}

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
EXPOSE 8090 8091
ENTRYPOINT ["/app"]

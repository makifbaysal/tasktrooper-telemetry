# Build context: repo root (ADR-027)  ·  docker build -f services/api/Dockerfile -t tasktrooper/api .
# services/pkg and sdks/go are copied for the go.mod replace directives (../pkg, ../../sdks/go).
FROM golang:1.26 AS build
WORKDIR /src
COPY services/pkg/ services/pkg/
COPY sdks/go/ sdks/go/
COPY services/api/go.mod services/api/go.sum services/api/
WORKDIR /src/services/api
RUN GOWORK=off go mod download
COPY services/api/ .
RUN CGO_ENABLED=0 GOWORK=off go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/api /api
EXPOSE 8080
ENTRYPOINT ["/api"]

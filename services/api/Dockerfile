# Build context: services/  ·  docker build -f api/Dockerfile -t tasktrooper/api services/
FROM golang:1.26 AS build
WORKDIR /src
COPY pkg/ pkg/
COPY api/go.mod api/go.sum api/
WORKDIR /src/api
RUN GOWORK=off go mod download
COPY api/ .
RUN CGO_ENABLED=0 GOWORK=off go build -trimpath -ldflags="-s -w" -o /out/api ./cmd/api

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/api /api
EXPOSE 8080
ENTRYPOINT ["/api"]

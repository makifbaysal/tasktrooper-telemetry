# Build context: services/  ·  docker build -f alerting/Dockerfile --build-arg CMD=alert-engine services/
FROM golang:1.26 AS build
ARG CMD=alert-engine
WORKDIR /src
COPY pkg/ pkg/
COPY alerting/go.mod alerting/go.sum alerting/
WORKDIR /src/alerting
RUN GOWORK=off go mod download
COPY alerting/ .
RUN CGO_ENABLED=0 GOWORK=off go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${CMD}

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
EXPOSE 8090 8091
ENTRYPOINT ["/app"]

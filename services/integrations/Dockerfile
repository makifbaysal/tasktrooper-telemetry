# Build context: repo root (ADR-027; the integrations module depends on ../pkg; the Go SDK comes from proxy.golang.org)
#   docker build -f services/integrations/Dockerfile -t trooper-cloud .
FROM golang:1.26-alpine AS build
WORKDIR /src
ENV GOWORK=off CGO_ENABLED=0
COPY services/pkg/ services/pkg/
COPY services/integrations/go.mod services/integrations/go.sum services/integrations/
RUN cd services/integrations && go mod download
COPY services/integrations/ services/integrations/
ARG VERSION=dev
RUN cd services/integrations && go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/trooper-cloud ./cmd/trooper-cloud

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/trooper-cloud /usr/local/bin/trooper-cloud
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/trooper-cloud"]

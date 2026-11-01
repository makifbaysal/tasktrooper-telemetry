# Build context: repo root (ADR-027; the collector module depends on ../pkg; the Go SDK comes from proxy.golang.org)
#   docker build -f services/collector/Dockerfile -t trooper-agent .
FROM golang:1.26-alpine AS build
WORKDIR /src
ENV GOWORK=off CGO_ENABLED=0
COPY services/pkg/ services/pkg/
COPY services/collector/go.mod services/collector/go.sum services/collector/
RUN cd services/collector && go mod download
COPY services/collector/ services/collector/
ARG VERSION=dev
RUN cd services/collector && go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/trooper-agent ./cmd/trooper-agent

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/trooper-agent /usr/local/bin/trooper-agent
# numeric (distroless nonroot = 65532) so runAsNonRoot can be verified by the kubelet
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/trooper-agent"]

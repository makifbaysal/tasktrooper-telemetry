# Build context: services/ (the collector module depends on ../pkg)
#   docker build -f services/collector/Dockerfile -t trooper-agent services/
FROM golang:1.26-alpine AS build
WORKDIR /src
ENV GOWORK=off CGO_ENABLED=0
COPY pkg/ pkg/
COPY collector/go.mod collector/go.sum collector/
RUN cd collector && go mod download
COPY collector/ collector/
ARG VERSION=dev
RUN cd collector && go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/trooper-agent ./cmd/trooper-agent

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/trooper-agent /usr/local/bin/trooper-agent
# numeric (distroless nonroot = 65532) so runAsNonRoot can be verified by the kubelet
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/trooper-agent"]

# Build context: repo root (ADR-027; the loadtest module depends on ../pkg and ../../sdks/go)
#   docker build -f services/loadtest/Dockerfile -t tasktrooper/loadtest .
# One image, two binaries: loadtest-ctl (entrypoint) and loadtest-runner (the k8s executor's Job command;
# the local executor finds it next to loadtest-ctl).
FROM golang:1.26-alpine AS build
WORKDIR /src
ENV GOWORK=off CGO_ENABLED=0
COPY services/pkg/ services/pkg/
COPY sdks/go/ sdks/go/
COPY services/loadtest/go.mod services/loadtest/go.sum services/loadtest/
RUN cd services/loadtest && go mod download
COPY services/loadtest/ services/loadtest/
ARG VERSION=dev
RUN cd services/loadtest && \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/loadtest-ctl ./cmd/loadtest-ctl && \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/loadtest-runner ./cmd/loadtest-runner

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/loadtest-ctl /out/loadtest-runner /usr/local/bin/
USER nonroot:nonroot
EXPOSE 8097
ENTRYPOINT ["/usr/local/bin/loadtest-ctl"]

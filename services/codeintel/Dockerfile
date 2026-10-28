# Build context: services/ (the codeintel module depends on ../pkg)
#   docker build -f services/codeintel/Dockerfile -t codeintel services/
FROM golang:1.26-alpine AS build
WORKDIR /src
ENV GOWORK=off CGO_ENABLED=0
COPY pkg/ pkg/
COPY codeintel/go.mod codeintel/go.sum codeintel/
RUN cd codeintel && go mod download
COPY codeintel/ codeintel/
RUN cd codeintel && go build -trimpath -ldflags "-s -w" -o /out/codeintel ./cmd/codeintel

# Runtime needs the git CLI (shallow clones), so alpine instead of distroless.
FROM alpine:3.22
RUN apk add --no-cache git ca-certificates && adduser -D -u 10001 codeintel && mkdir -p /work && chown codeintel /work
COPY --from=build /out/codeintel /usr/local/bin/codeintel
USER codeintel
ENV TT_CODEINTEL_WORKDIR=/work
EXPOSE 8096
ENTRYPOINT ["/usr/local/bin/codeintel"]

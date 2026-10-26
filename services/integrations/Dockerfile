# Build context: services/ (the integrations module depends on ../pkg)
#   docker build -f services/integrations/Dockerfile -t trooper-cloud services/
FROM golang:1.26-alpine AS build
WORKDIR /src
ENV GOWORK=off CGO_ENABLED=0
COPY pkg/ pkg/
COPY integrations/go.mod integrations/go.sum integrations/
RUN cd integrations && go mod download
COPY integrations/ integrations/
ARG VERSION=dev
RUN cd integrations && go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/trooper-cloud ./cmd/trooper-cloud

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/trooper-cloud /usr/local/bin/trooper-cloud
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/trooper-cloud"]

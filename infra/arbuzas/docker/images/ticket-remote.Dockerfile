# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.24-bookworm AS build

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src/workloads/ticket-remote

COPY workloads/ticket-remote/go.mod workloads/ticket-remote/go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
  go mod download

COPY workloads/ticket-remote ./

RUN --mount=type=cache,target=/go/pkg/mod \
  --mount=type=cache,target=/root/.cache/go-build \
  CGO_ENABLED=0 GOOS="${TARGETOS:-linux}" GOARCH="${TARGETARCH:-amd64}" \
    go build -o /out/ticket-remote ./cmd/ticket-remote

FROM --platform=$TARGETPLATFORM debian:bookworm-slim

RUN apt-get update \
  && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends ca-certificates curl \
  && rm -rf /var/lib/apt/lists/*

WORKDIR /srv/ticket-remote

COPY --from=build /out/ticket-remote /usr/local/bin/ticket-remote

CMD ["/usr/local/bin/ticket-remote"]

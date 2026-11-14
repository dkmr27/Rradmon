# syntax=docker/dockerfile:1
#
# Rradmon server image. Pure Go, so it cross-compiles for every platform
# from the build machine:
#
#   docker build -t rradmon .
#   docker buildx build --platform linux/amd64,linux/arm64,linux/arm/v7,linux/arm/v6 -t rradmon .
#
# The database lives in /data (a volume). See the README, "Docker".

ARG GO_VERSION=1.27

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS build
ARG TARGETOS TARGETARCH TARGETVARIANT
ARG VERSION=0.0.0-docker
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN case "$TARGETVARIANT" in v6) export GOARM=6 ;; v7) export GOARM=7 ;; esac; \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -buildid= -X main.version=$VERSION" -o /out/rradmon ./cmd/rradmon \
    && mkdir -p /out/data

FROM scratch
# CA certificates for MQTT over TLS (ssl://, wss://)
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/rradmon /rradmon
COPY --from=build --chown=65532:65532 /out/data /data
# Not root. For a serial device, add its group: --group-add <gid of the device>
USER 65532:65532
WORKDIR /data
# Listen on all interfaces inside the container; the host's port mapping
# decides who can reach it
ENV RRADMON_HTTP=0.0.0.0:8080
EXPOSE 8080
VOLUME /data
ENTRYPOINT ["/rradmon"]
CMD ["help"]

FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG VERSION=dev
ARG INCLUDE_CLIENTS=true

# Build router binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X main.version=${VERSION}" -o /router ./router/cmd/router/

# Ensure /dist and /downloads exist (clients or empty dirs)
RUN mkdir -p /dist /downloads

# Build client binaries for multiple platforms (only when INCLUDE_CLIENTS is true)
RUN if [ "${INCLUDE_CLIENTS}" = "true" ]; then \
      CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 go build -ldflags="-s -w -X main.version=${VERSION}" -o /dist/llmesh-client-linux-amd64  ./client/cmd/client/ && \
      CGO_ENABLED=0 GOOS=linux  GOARCH=arm64 go build -ldflags="-s -w -X main.version=${VERSION}" -o /dist/llmesh-client-linux-arm64  ./client/cmd/client/ && \
      CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags="-s -w -X main.version=${VERSION}" -o /dist/llmesh-client-darwin-amd64 ./client/cmd/client/ && \
      CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags="-s -w -X main.version=${VERSION}" -o /dist/llmesh-client-darwin-arm64 ./client/cmd/client/; \
    fi

# Build shim binaries for multiple platforms (only when INCLUDE_CLIENTS is true)
RUN if [ "${INCLUDE_CLIENTS}" = "true" ]; then \
      CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 go build -ldflags="-s -w -X main.version=${VERSION}" -o /dist/llmesh-shim-linux-amd64  ./shim/cmd/shim/ && \
      CGO_ENABLED=0 GOOS=linux  GOARCH=arm64 go build -ldflags="-s -w -X main.version=${VERSION}" -o /dist/llmesh-shim-linux-arm64  ./shim/cmd/shim/ && \
      CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -ldflags="-s -w -X main.version=${VERSION}" -o /dist/llmesh-shim-darwin-amd64 ./shim/cmd/shim/ && \
      CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -ldflags="-s -w -X main.version=${VERSION}" -o /dist/llmesh-shim-darwin-arm64 ./shim/cmd/shim/; \
    fi

# Prepare /downloads with ancillary files (only when INCLUDE_CLIENTS is true)
RUN if [ "${INCLUDE_CLIENTS}" = "true" ]; then \
      mkdir -p /downloads && \
      cp client/man/llmesh-client.1 /downloads/ && \
      cp router/config.yaml.example /downloads/ && \
      cp shim/man/llmesh-shim.1 /downloads/ && \
      cp shim/config.yaml.example /downloads/llmesh-shim-config.yaml.example; \
    fi

FROM alpine:3.19
RUN apk add --no-cache ca-certificates wget
RUN mkdir -p /downloads
COPY --from=builder /router /router
COPY --from=builder /dist /downloads
COPY --from=builder /downloads /downloads
# Run as an unprivileged user. /data (the state database volume) is owned by
# it so named volumes work out of the box; for bind mounts, chown the host
# directory to uid 10001 (rootful docker) or map your user onto uid 10001
# with `--userns=keep-id:uid=10001,gid=10001` (rootless podman).
RUN adduser -D -u 10001 llmesh && mkdir -p /data && chown llmesh:llmesh /data
USER llmesh
# Assumes the default listen port (53002). If server.port is changed, override
# with a compose-level healthcheck.
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -q -O /dev/null http://127.0.0.1:53002/health || exit 1
ENTRYPOINT ["/router"]

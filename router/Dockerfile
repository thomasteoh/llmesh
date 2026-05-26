FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG VERSION=dev
ARG INCLUDE_CLIENTS=true

# Build router binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X main.version=${VERSION}" -o /router ./router/cmd/router/

# Ensure /dist exists (clients or empty dir)
RUN mkdir -p /dist

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
      cp client/docker-compose.client.yml /downloads/ && \
      cp router/config.yaml.example /downloads/ && \
      cp shim/man/llmesh-shim.1 /downloads/ && \
      cp shim/docker-compose.shim.yml /downloads/ && \
      cp shim/config.yaml.example /downloads/llmesh-shim-config.yaml.example; \
    fi

FROM alpine:3.19
RUN apk add --no-cache ca-certificates
RUN mkdir -p /downloads
COPY --from=builder /router /router
COPY --from=builder /dist /downloads
COPY --from=builder /downloads /downloads
ENTRYPOINT ["/router"]

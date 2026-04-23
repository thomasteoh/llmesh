FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
COPY vendor ./vendor
COPY . .

ARG VERSION=dev

RUN CGO_ENABLED=0 GOOS=linux go build -mod=vendor -ldflags="-s -w -X main.version=${VERSION}" -o /router ./router/cmd/router/

RUN CGO_ENABLED=0 GOOS=linux  GOARCH=amd64 go build -mod=vendor -ldflags="-s -w -X main.version=${VERSION}" -o /dist/llm-client-linux-amd64  ./client/cmd/client/
RUN CGO_ENABLED=0 GOOS=linux  GOARCH=arm64 go build -mod=vendor -ldflags="-s -w -X main.version=${VERSION}" -o /dist/llm-client-linux-arm64  ./client/cmd/client/
RUN CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -mod=vendor -ldflags="-s -w -X main.version=${VERSION}" -o /dist/llm-client-darwin-amd64 ./client/cmd/client/
RUN CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -mod=vendor -ldflags="-s -w -X main.version=${VERSION}" -o /dist/llm-client-darwin-arm64 ./client/cmd/client/

FROM alpine:3.19
RUN apk add --no-cache ca-certificates
RUN mkdir -p /downloads
COPY --from=builder /router /router
COPY --from=builder /dist /downloads
COPY client/man/llm-client.1 /downloads/llm-client.1
COPY client/docker-compose.client.yml /downloads/docker-compose.client.yml
COPY router/config.yaml.example /downloads/config.yaml.example
ENTRYPOINT ["/router"]

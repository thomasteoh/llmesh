FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X main.version=${VERSION}" -o /client ./client/cmd/client/

FROM alpine:3.19
RUN apk add --no-cache ca-certificates man-db less
COPY --from=builder /client /client
COPY client/man/llmesh-client.1 /usr/share/man/man1/llmesh-client.1
RUN gzip /usr/share/man/man1/llmesh-client.1
# Run as an unprivileged user. The client mounts config read-only and writes no
# state to disk, so no writable paths need to be owned.
RUN adduser -D -u 10001 llmesh
USER llmesh
ENTRYPOINT ["/client"]

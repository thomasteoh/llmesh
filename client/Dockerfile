FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X main.version=${VERSION}" -o /client ./client/cmd/client/

FROM alpine:3.19
RUN apk add --no-cache ca-certificates man-db less
COPY --from=builder /client /client
COPY client/man/llm-client.1 /usr/share/man/man1/llm-client.1
RUN gzip /usr/share/man/man1/llm-client.1
ENTRYPOINT ["/client"]
